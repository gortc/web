package main

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc64"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/gortc/ice"
	"github.com/gortc/sdp"
	"github.com/gortc/stun"
	"github.com/mssola/user_agent"
)

var (
	portHTTP = flag.Int("port", 3000, "http server port")
	hostHTTP = flag.String("host", "localhost", "http server host")
	portSTUN = flag.Int("port-stun", stun.DefaultPort, "UDP port")
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
		IP:   ip,
		Port: port,
	}.AddTo(res)
	stun.NewSoftware("gortc.io/x/sdp example").AddTo(res)
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

func main() {
	flag.Parse()
	cf, err := cloudflare.New(
		os.Getenv("CF_API_KEY"),
		os.Getenv("CF_API_EMAIL"),
	)
	if err != nil {
		log.Fatal(err)
	}
	t, err := template.ParseFiles("static/index.html")
	if err != nil {
		log.Fatal(err)
	}
	log.SetFlags(log.Lshortfile)
	fs := http.FileServer(http.Dir("static"))
	var (
		s     *stats
		sLock sync.RWMutex
	)
	go func() {
		sLock.Lock()
		log.Println("gettings stats")
		s, err = getStats(false)
		if err != nil {
			log.Println("failed to get stats:", err)
		} else {
			log.Println("got stats")
		}
		sLock.Unlock()
	}()
	update := func() error {
		log.Println("updating stats")
		newStats, err := getStats(true)
		if err != nil {
			log.Println("failed to fetch stats:", err)
			return err
		}
		sLock.Lock()
		s = newStats
		sLock.Unlock()
		return nil
	}
	go func() {
		ticker := time.NewTicker(time.Second * 90)
		for range ticker.C {
			_ = update()
		}
	}()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			fs.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Link", "</go-rtc.svg>; as=image; rel=preload")
		w.Header().Add("Link", "</jetbrains-variant-3.svg>; as=image; rel=preload")
		w.Header().Add("Link", "</css/main.css>; as=style; rel=preload")
		sLock.RLock()
		if err := t.Execute(w, s); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, err)
		}
		sLock.RUnlock()
	})
	http.HandleFunc("/hook/"+os.Getenv("GITHUB_HOOK_SECRET"), func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		err := update()
		if err != nil {
			writer.WriteHeader(http.StatusInternalServerError)
			fmt.Println(writer, "failed:", err)
			return
		}
		writer.WriteHeader(http.StatusOK)
		fmt.Fprintln(writer, "updated in", time.Since(start))
		go func() {
			log.Println("purging cf cache")
			zoneID, err := cf.ZoneIDByName("gortc.io")
			if err != nil {
				log.Println("failed to get zone id:", err)
				return
			}
			res, err := cf.PurgeCache(zoneID, cloudflare.PurgeCacheRequest{
				Files: []string{
					"https://gortc.io/",
				},
			})
			if err != nil {
				log.Println("failed to purge cache:", err)
				return
			}
			if !res.Success {
				log.Println("failed to purge cache: not succeeded")
			} else {
				log.Println("purged cf cache")
			}
		}()
	})
	http.HandleFunc("/ice-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-type", "application/json")
		encoder := json.NewEncoder(w)
		origin := r.Header.Get("Origin")
		server := "stun:gortc.io"
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

	// Custom domain support.
	for _, name := range []string{
		"stun", "turn", "sdp", "web", "ice", "api",
	} {
		body := strings.Replace(`<!DOCTYPE html>
<html>
<head>
    <meta http-equiv="Content-Type" content="text/html; charset=utf-8"/>
    <meta name="go-import" content="gortc.io/pkg git https://github.com/gortc/pkg.git">
    <meta name="go-source"
          content="gortc.io/pkg https://github.com/gortc/pkg/ https://github.com/gortc/pkg/tree/master{/dir} https://github.com/gortc/pkg/blob/master{/dir}/{file}#L{line}">
    <meta http-equiv="refresh" content="0; url=https://godoc.org/gortc.io/pkg">
</head>
<body>
Nothing to see here; <a href="https://godoc.org/gortc.io/pkg">move along</a>.
</body>`, "pkg", name, -1)
		http.HandleFunc("/"+name+"/", func(w http.ResponseWriter, r *http.Request) {
			defer r.Body.Close()
			fmt.Fprint(w, body)
		})
	}

	http.HandleFunc("/x/sdp", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		log.Println("http:", r.Method, "request from", r.RemoteAddr)
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/x/sdp/", http.StatusPermanentRedirect)
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
