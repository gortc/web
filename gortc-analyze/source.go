package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

var (
	fetch = flag.Bool("fetch", false, "fetch")
)

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

func main() {
	flag.Parse()
	n := time.Now()
	last30Days := n.AddDate(0, 0, -30)
	last7Days := n.AddDate(0, 0, -7)
	last24h := n.AddDate(0, 0, -1)
	var total, last30DaysTotal, last7DaysTotal, last24hTotal int
	for _, name := range []string{
		"stun", "turn", "sdp", "web", "stund", "tech-status",
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
			if *fetch {
				err = r.Fetch(&git.FetchOptions{
					Force: true,
				})
				if err == git.NoErrAlreadyUpToDate {
					err = nil
				}
			}
		}
		if err != nil {
			log.Fatal(err)
		}
		rep, err := getTokeiReport(p)
		if err != nil {
			log.Fatal(err)
		}
		ref, err := r.Head()
		if err != nil {
			log.Fatal(err)
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
			log.Fatal(err)
		}
		fmt.Println(name, "commits:", commits)
		fmt.Println(name, "lines", rep.Go.Lines)
		total += commits
	}
	fmt.Println("total:", total)
	fmt.Println("last 30 days:", last30DaysTotal)
	fmt.Println("last 7 days:", last7DaysTotal)
	fmt.Println("last 24h:", last24hTotal)
}
