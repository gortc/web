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
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ernado/ice"
	"github.com/gortc/sdp"
	"github.com/gortc/stun"
	"github.com/mssola/user_agent"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
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
		IP:   ip,
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

type stats struct {
	Total   int
	Last30d int
	Last7d  int
	Last24h int
	Lines   int
}

type tokeiLanguageReport struct {
	Blanks   int `json:"blanks"`
	Code     int `json:"code"`
	Comments int `json:"comments"`
	Stats    []struct {
		Blanks   int    `json:"blanks"`
		Code     int    `json:"code"`
		Comments int    `json:"comments"`
		Lines    int    `json:"lines"`
		Name     string `json:"name"`
	} `json:"stats"`
	Lines int `json:"lines"`
}

type TokeiReport struct {
	CSS        tokeiLanguageReport `json:"Css"`
	Dockerfile tokeiLanguageReport `json:"Dockerfile"`
	Go         tokeiLanguageReport `json:"Go"`
	Hex        tokeiLanguageReport `json:"Hex"`
	HTML       tokeiLanguageReport `json:"Html"`
	Makefile   tokeiLanguageReport `json:"Makefile"`
	Markdown   tokeiLanguageReport `json:"Markdown"`
	Sh         tokeiLanguageReport `json:"Sh"`
	Text       tokeiLanguageReport `json:"Text"`
	Toml       tokeiLanguageReport `json:"Toml"`
	Yaml       tokeiLanguageReport `json:"Yaml"`
}

func getTokeiReport(path string) (*TokeiReport, error) {
	cmd := exec.Command("tokei", "-e", "vendor/", path, "-o", "json")
	buf := new(bytes.Buffer)
	cmd.Stdout = buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(buf)
	r := new(TokeiReport)
	if err := decoder.Decode(r); err != nil {
		return nil, err
	}
	return r, nil
}

func getStats(fetch bool) (*stats, error) {
	n := time.Now()
	last30Days := n.AddDate(0, 0, -30)
	last7Days := n.AddDate(0, 0, -7)
	last24h := n.AddDate(0, 0, -1)
	var total, last30DaysTotal, last7DaysTotal, last24hTotal, lines int
	for _, name := range []string{
		"stun", "turn", "sdp", "web", "stund", "tech-status", "ice", "rtc",
	} {
		p := "/tmp/gortc-analyze/" + name
		r, err := git.PlainClone(p, false, &git.CloneOptions{
			URL: "https://github.com/gortc/" + name,
		})
		if err == git.ErrRepositoryAlreadyExists {
			r, err = git.PlainOpen(p)
			if err != nil {
				log.Fatal(err)
			}
			if fetch {
				err = r.Fetch(&git.FetchOptions{
					Force: true,
				})
				if err == git.NoErrAlreadyUpToDate {
					err = nil
				}
			}
		}
		if err != nil {
			return nil, err
		}
		rep, err := getTokeiReport(p)
		if err != nil {
			return nil, err
		}
		lines += rep.Go.Lines
		ref, err := r.Head()
		if err != nil {
			return nil, err
		}
		b, err := r.Log(&git.LogOptions{
			From: ref.Hash(),
		})
		var commits int
		if err = b.ForEach(func(commit *object.Commit) error {
			commits++
			if commit.Author.When.After(last30Days) {
				last30DaysTotal++
			}
			if commit.Author.When.After(last7Days) {
				last7DaysTotal++
			}
			if commit.Author.When.After(last24h) {
				last24hTotal++
			}
			return nil
		}); err != nil {
			return nil, err
		}
		total += commits
	}
	return &stats{
		Total:   total,
		Last30d: last30DaysTotal,
		Last7d:  last7DaysTotal,
		Last24h: last24hTotal,
		Lines:   lines,
	}, nil
}

func main() {
	flag.Parse()
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
	log.Println("gettings stats")
	s, err = getStats(false)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("got stats")
	update := func() {
		log.Println("updating stats")
		newStats, err := getStats(true)
		if err != nil {
			log.Println("failed to fetch stats:", err)
		}
		sLock.Lock()
		s = newStats
		sLock.Unlock()
	}
	go func() {
		ticker := time.NewTicker(time.Second * 90)
		for range ticker.C {
			update()
		}
	}()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			fs.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Link", "</go-rtc.svg>; as=image; rel=preload")
		sLock.RLock()
		if err := t.Execute(w, s); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, err)
		}
		sLock.RUnlock()
	})
	http.HandleFunc("/hook", func(writer http.ResponseWriter, request *http.Request) {
		// TODO(ar): check secret
		update()
		writer.WriteHeader(http.StatusOK)
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
		"stun", "turn", "sdp", "web", "ice",
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
