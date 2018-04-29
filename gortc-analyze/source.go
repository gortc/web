package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
)

var (
	fetch = flag.Bool("fetch", false, "fetch")
)

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
		total += commits
	}
	fmt.Println("total:", total)
	fmt.Println("last 30 days:", last30DaysTotal)
	fmt.Println("last 7 days:", last7DaysTotal)
	fmt.Println("last 24h:", last24hTotal)
}
