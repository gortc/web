package main

import (
	"log"
	"sync"
	"time"

	"github.com/gortc/stun"
)

var messages = &storage{
	data: make(map[string]*storageEntry),
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
