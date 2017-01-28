package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ernado/ice"
	"github.com/ernado/sdp"
	"github.com/ernado/stun"
	"github.com/pkg/errors"
	"hash/crc64"
	"net/url"
	"strings"
)

var (
	portHTTP = flag.Int("port", 3000, "http server portHTTP")
	hostHTTP = flag.String("host", "localhost", "http server host")
	portSTUN = flag.Int("port-stun", stun.DefaultPort, "UDP portHTTP for STUN")
	messages = &storage{
		data: make(map[string]*storageEntry),
	}
)

func processUDPPacket(addr net.Addr, b []byte, req, res *stun.Message) error {
	if !stun.IsMessage(b) {
		log.Println("packet from", addr, "is not STUN message")
		return nil
	}
	if _, err := req.ReadBytes(b); err != nil {
		return errors.Wrap(err, "failed to read message")
	}
	res.TransactionID = req.TransactionID
	res.Type = stun.MessageType{
		Method: stun.MethodBinding,
		Class:  stun.ClassSuccessResponse,
	}
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
	res.AddXORMappedAddress(ip, port)
	res.AddSoftware("cydev.ru/sdp/ example")
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
	return time.Now().Add(time.Second * -4)
}

func (s *storage) pop(addr string) *stun.Message {
	s.Lock()
	if s.data[addr] == nil {
		return nil
	}
	m := s.data[addr].Clone()
	stun.ReleaseMessage(s.data[addr].Message)
	delete(s.data, addr)
	s.Unlock()
	return m
}

func (s *storage) add(addr string, m *stun.Message) {
	entry := &storageEntry{
		Message:   m.Clone(),
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
		stun.ReleaseMessage(s.data[addr].Message)
		delete(s.data, addr)
	}
	s.Unlock()
	if len(toRemove) > 0 {
		log.Println("storage: collected", len(toRemove))
	}
}

func (s *storage) collectCycle() {
	ticker := time.NewTicker(time.Second * 2)
	for range ticker.C {
		s.collect()
	}
}

func main() {
	flag.Parse()
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/", fs)
	http.HandleFunc("/ice-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		origin := r.Header.Get("Origin")
		server := "stun:a1.cydev.ru"
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
	http.HandleFunc("/sdp", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		log.Println("http: got request from", r.RemoteAddr)
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
			fmt.Fprintf(w, "<p>%02d %s</p>\n", k, v)
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
			fmt.Fprintln(w, `<p>dumped: <code>stun-decode`, base64.StdEncoding.EncodeToString(m.Bytes()), `</code></p>`)
			fmt.Fprintln(w, `<p>crc64: <code>`, crc64.Checksum(m.Bytes(), crc64.MakeTable(crc64.ISO)), `</code></p>`)
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
	go messages.collectCycle()
	go func(conn net.PacketConn) {
		log.Println("Started STUN server on", conn.LocalAddr())
		var (
			res = stun.AcquireMessage()
			req = stun.AcquireMessage()
			buf = make([]byte, stun.MaxPacketSize)
		)
		defer stun.ReleaseMessage(res)
		defer stun.ReleaseMessage(req)
		for {
			n, addr, err := c.ReadFrom(buf)
			if err != nil {
				log.Fatalln("c.ReadFrom:", err)
			}
			log.Printf("udp: got packet len(%d) from %s", n, addr)
			if err = processUDPPacket(addr, buf[:n], req, res); err != nil {
				log.Println("failed to process UDP packet:", err, "from addr", addr)
			} else {
				log.Printf("stun: parsed message %q from %s", req, addr)
				if _, err = c.WriteTo(res.Bytes(), addr); err != nil {
					log.Println("failed to send packet:", err)
				}
			}
			res.Reset()
			req.Reset()
			for i := range buf[:n] {
				buf[i] = 0
			}
		}

	}(c)
	log.Println("Listening http", addrHTTP)
	log.Fatal(http.ListenAndServe(addrHTTP, nil))
}
