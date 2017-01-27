package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"

	"github.com/ernado/sdp"
)

func main() {
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/", fs)
	http.HandleFunc("/sdp", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		s := sdp.Session{}
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Println(err)
			return
		}
		if s, err = sdp.DecodeSession(data, s); err != nil {
			log.Println(err)
			return
		}
		for k, v := range s {
			fmt.Fprintf(w, "<p>%02d %s</p>\n", k, v)
		}
	})

	log.Println("Listening")
	http.ListenAndServe(":3000", nil)
}
