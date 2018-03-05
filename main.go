package main

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc64"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mssola/user_agent"

	"github.com/ernado/ice"
	"github.com/gortc/sdp"
	"github.com/gortc/stun"
)

var (
	portHTTP = flag.Int("port", 3000, "http server port")
	hostHTTP = flag.String("host", "localhost", "http server host")
	portSTUN = flag.Int("port-stun", stun.DefaultPort, "UDP port")
	messages = &storage{
		data: make(map[string]*storageEntry),
	}
)

var (
	bindingRequest = stun.MessageType{
		Method: stun.MethodBinding,
		Class:  stun.ClassRequest,
	}
	bindingSuccessResponse = stun.MessageType{
		Method: stun.MethodBinding,
		Class:  stun.ClassSuccessResponse,
	}
)

func processUDPPacket(addr net.Addr, b []byte, req, res *stun.Message) error {
	if !stun.IsMessage(b) {
		log.Println("packet from", addr, "is not STUN message")
		return nil
	}
	req.Raw = b
	if err := req.Decode(); err != nil {
		return err
	}
	if req.Type != bindingRequest {
		log.Println("stun: skipping", req.Type, "from", addr)
		return nil
	}
	log.Println("stun: got", req.Type)
	res.TransactionID = req.TransactionID
	res.Type = bindingSuccessResponse
	var (
		ip   net.IP
		port int
	)
	switch a := addr.(type) {
	case *net.UDPAddr:
		ip = a.IP
		port = a.Port
	default:
		panic(fmt.Sprintf("unknown addr: %v", addr))
	}
	stun.XORMappedAddress{
		IP: ip,
		Port: port,
	}.AddTo(res)
	stun.NewSoftware("cydev.ru/sdp example").AddTo(res)
	res.WriteHeader()
	messages.add(fmt.Sprintf("%s:%d", ip, port), req)
	return nil
}

type iceServerConfiguration struct {
	URLs []string `json:"urls"`
}

type iceConfiguration struct {
	Servers []iceServerConfiguration `json:"iceServers"`
}

type storageEntry struct {
	*stun.Message
	createdAt time.Time
}

func (s storageEntry) timedOut(timeout time.Time) bool {
	return s.createdAt.Before(timeout)
}

type storage struct {
	data map[string]*storageEntry
	sync.Mutex
}

func (storage) timeout() time.Time {
	return time.Now().Add(time.Second * -60)
}

func mustClone(m *stun.Message) *stun.Message {
	b := new(stun.Message)
	if err := m.CloneTo(b); err != nil {
		panic(err)
	}
	return b
}

func (s *storage) pop(addr string) *stun.Message {
	s.Lock()
	defer s.Unlock()
	if s.data[addr] == nil {
		return nil
	}
	m := mustClone(s.data[addr].Message)
	delete(s.data, addr)
	return m
}

func (s *storage) add(addr string, m *stun.Message) {
	c := new(stun.Message)
	m.CloneTo(c)
	entry := &storageEntry{
		Message:   c,
		createdAt: time.Now(),
	}
	s.Lock()
	s.data[addr] = entry
	s.Unlock()
	log.Println("storage: added", addr)
}

func (s *storage) collect() {
	s.Lock()
	var (
		toRemove = make([]string, 0, 10)
		timeout  = s.timeout()
	)
	for addr, m := range s.data {
		if m.timedOut(timeout) {
			toRemove = append(toRemove, addr)
		}
	}
	for _, addr := range toRemove {
		delete(s.data, addr)
	}
	s.Unlock()
	if len(toRemove) > 0 {
		log.Println("storage: collected", len(toRemove))
	}
}

func (s *storage) gc() {
	ticker := time.NewTicker(time.Second * 2)
	for range ticker.C {
		s.collect()
	}
}

func main() {
	flag.Parse()
	log.SetFlags(log.Lshortfile)
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/", fs)
	http.HandleFunc("/ice-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-type", "application/json")
		encoder := json.NewEncoder(w)
		origin := r.Header.Get("Origin")
		server := "stun:rs.cydev.ru"
		if len(origin) > 0 {
			u, err := url.Parse(origin)
			if err != nil {
				log.Printf("http: failed to parse origin %q: %s", origin, err)
			} else {
				if idx := strings.LastIndex(u.Host, ":"); idx > 0 {
					u.Host = u.Host[:idx]
				}
				server = fmt.Sprintf("stun:%s:%d", u.Host, *portSTUN)
				log.Printf("http: sending ice-server %q for origin %q", server, origin)
			}
		}
		if err := encoder.Encode(iceConfiguration{
			Servers: []iceServerConfiguration{
				{URLs: []string{server}},
			},
		}); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, "json encode:", err)
		}
	})

	mLog, err := os.OpenFile("packets.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		log.Fatalln("Failed to open log:", err)
	}
	defer mLog.Close()
	csvLog := csv.NewWriter(mLog)

	http.HandleFunc("/sdp", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		log.Println("http:", r.Method, "request from", r.RemoteAddr)
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/sdp/", http.StatusPermanentRedirect)
			return
		}
		s := sdp.Session{}
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Println("http: ReadAll body failed:", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if s, err = sdp.DecodeSession(data, s); err != nil {
			log.Println("http: failed to decode sdp session:", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for k, v := range s {
			fmt.Fprintf(w, `<p class="attribute">%02d %s</p>`+"\n", k, v)
			if v.Type != sdp.TypeAttribute {
				continue
			}
			if !bytes.HasPrefix(v.Value, []byte("candidate")) {
				continue
			}
			c := new(ice.Candidate)
			fmt.Fprintln(w, `<div class="stun-message">`)
			if err = ice.ParseAttribute(v.Value, c); err != nil {
				fmt.Fprintln(w, `<p class="error">failed to parse as candidate:`, err, "</p>")
				fmt.Fprintln(w, `</div>`)
				continue
			}
			fmt.Fprintln(w, "<p>parsed as candidate:", fmt.Sprintf("%+v", c), "</p>")
			if c.Type != ice.CandidateServerReflexive {
				fmt.Fprintln(w, `</div>`)
				continue
			}
			addr := fmt.Sprintf("%s:%d", c.ConnectionAddress, c.Port)
			m := messages.pop(addr)
			if m == nil {
				log.Println("http: no message for", addr, "in log")
				fmt.Fprintln(w, `<p class="warning">message from candidate not found in STUN log</p>`)
				fmt.Fprintln(w, `</div>`)
				continue
			}
			fmt.Fprintln(w, `<p class="success">message found in STUN log:`, m, `</p>`)
			for _, a := range m.Attributes {
				switch a.Type {
				case stun.AttrOrigin:
					fmt.Fprintf(w, "<p>STUN attribute %s: %q (len=%d)</p>", a.Type, a.Value, a.Length)
				default:
					fmt.Fprintf(w, "<p>STUN attribute %s: %v (len=%d)</p>", a.Type, a.Value, a.Length)
				}
			}
			var (
				b64             = base64.StdEncoding.EncodeToString(m.Raw)
				messageCRC64    = crc64.Checksum(m.Raw, crc64.MakeTable(crc64.ISO))
				clipID          = fmt.Sprintf("crc64-%d", messageCRC64)
				ua              = user_agent.New(r.Header.Get("User-agent"))
				bName, bVersion = ua.Browser()
			)
			if err = csvLog.Write([]string{
				addr,
				b64,
				fmt.Sprintf("%d", messageCRC64),
				bName,
				bVersion,
				ua.OS(),
			}); err != nil {
				log.Fatalln("log: failed to write:", err)
			}
			csvLog.Flush()
			fmt.Fprintln(w, `<p>dumped: <code id="`+clipID+`">stun-decode`, b64, `</code>
				<button class="btn" data-clipboard-target="#`+clipID+`">copy</button>
			</p>`)
			fmt.Fprintln(w, `<p>crc64: <code>`, messageCRC64, `</code></p>`)
			fmt.Fprintln(w, `</div>`)
		}
	})

	var (
		addrSTUN = fmt.Sprintf(":%d", *portSTUN)
		addrHTTP = fmt.Sprintf("%s:%d", *hostHTTP, *portHTTP)
	)
	log.Println("Listening udp", addrSTUN)
	c, err := net.ListenPacket("udp", addrSTUN)
	if err != nil {
		log.Fatalf("Failed to bind udp on %s: %s", addrSTUN, err)
	}
	defer c.Close()

	// spawning storage garbage collector
	go messages.gc()

	// spawning STUN server
	go func(conn net.PacketConn) {
		log.Println("Started STUN server on", conn.LocalAddr())
		var (
			res = new(stun.Message)
			req = new(stun.Message)
			buf = make([]byte, 1024)
		)
		for {
			// ReadFrom c to buf
			n, addr, err := c.ReadFrom(buf)
			if err != nil {
				log.Fatalln("c.ReadFrom:", err)
			}
			log.Printf("udp: got packet len(%d) from %s", n, addr)
			// processing binding request
			if err = processUDPPacket(addr, buf[:n], req, res); err != nil {
				log.Println("failed to process UDP packet:", err, "from addr", addr)
			} else {
				log.Printf("stun: parsed message %q from %s", req, addr)
				if _, err = c.WriteTo(res.Raw, addr); err != nil {
					log.Println("failed to send packet:", err)
				}
			}
			res.Reset()
			req.Reset()
		}

	}(c)
	log.Println("Listening http", addrHTTP)
	log.Fatal(http.ListenAndServe(addrHTTP, nil))
}
