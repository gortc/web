package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

type stats struct {
	Total   int
	Last30d int
	Last7d  int
	Last24h int
	Lines   int
	Time    time.Time
}

func (s stats) FormattedTime() string {
	return s.Time.In(time.UTC).Format(time.RFC850)
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
		"stun", "turn", "sdp", "web", "stund", "tech-status", "ice", "rtc", "gortcd",
		"ansible-role-nginx", "ansible-go", "api",
	} {
		p := "/tmp/gortc-analyze/" + name
		r, err := git.PlainClone(p, false, &git.CloneOptions{
			URL: "https://github.com/gortc/" + name,
		})
		if err == git.ErrRepositoryAlreadyExists {
			r, err = git.PlainOpen(p)
			if err != nil {
				return nil, err
			}
			w, err := r.Worktree()
			if err != nil {
				return nil, err
			}
			if fetch {
				err = w.Pull(&git.PullOptions{
					Force:      true,
					RemoteName: "origin",
				})
				log.Println("pull", name, err)
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
		lines += rep.Go.Lines + rep.Yaml.Lines
		ref, err := r.Head()
		if err != nil {
			return nil, err
		}
		fmt.Println(name, "head", ref)
		b, err := r.Log(&git.LogOptions{
			From: ref.Hash(),
		})
		var commits int
		countAuthors := map[string]struct{}{
			"ar@cydev.ru":     {},
			"ernado@ya.ru":    {},
			"mail@backkem.me": {},
		}
		if err = b.ForEach(func(commit *object.Commit) error {
			if _, ok := countAuthors[commit.Author.Email]; !ok {
				return nil
			}
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
		Time:    time.Now(),
		Total:   total,
		Last30d: last30DaysTotal,
		Last7d:  last7DaysTotal,
		Last24h: last24hTotal,
		Lines:   lines,
	}, nil
}
