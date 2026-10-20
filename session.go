package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"strconv"
	"sync"
	"time"
)

// ------------------------------------------------- one-time session tokens

const tokenTTL = 60 * time.Second

// tokenStore maps short-lived, single-use tokens to the connection they open.
// Credentials stay in server memory; the browser only ever sees the random token.
type tokenStore struct {
	mu      sync.Mutex
	pending map[string]pendingToken
}

type pendingToken struct {
	target *target
	exp    time.Time
}

func newTokenStore() *tokenStore {
	t := &tokenStore{pending: map[string]pendingToken{}}
	go func() {
		for range time.Tick(30 * time.Second) {
			now := time.Now()
			t.mu.Lock()
			for k, p := range t.pending {
				if p.exp.Before(now) {
					delete(t.pending, k)
				}
			}
			t.mu.Unlock()
		}
	}()
	return t
}

func (t *tokenStore) issue(tg *target) string {
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	t.mu.Lock()
	t.pending[tok] = pendingToken{target: tg, exp: time.Now().Add(tokenTTL)}
	t.mu.Unlock()
	return tok
}

// take consumes a token. It returns nil if the token is unknown, expired or already used.
func (t *tokenStore) take(tok string) *target {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pending[tok]
	if !ok {
		return nil
	}
	delete(t.pending, tok)
	if p.exp.Before(time.Now()) {
		return nil
	}
	return p.target
}

// ----------------------------------------------------------- reachability

type statusCache struct {
	mu      sync.Mutex
	entries map[int64]probeResult
}

type probeResult struct {
	at time.Time
	up bool
	ms *int64
}

type statusItem struct {
	ID int64  `json:"id"`
	Up bool   `json:"up"`
	Ms *int64 `json:"ms"`
}

func newStatusCache() *statusCache { return &statusCache{entries: map[int64]probeResult{}} }

func (s *statusCache) forget(id int64) {
	s.mu.Lock()
	delete(s.entries, id)
	s.mu.Unlock()
}

func probe(ctx context.Context, host string, port int) probeResult {
	start := time.Now()
	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return probeResult{at: start}
	}
	conn.Close()
	ms := time.Since(start).Milliseconds()
	return probeResult{at: start, up: true, ms: &ms}
}

// check probes every connection in parallel, reusing results younger than 15 seconds.
func (s *statusCache) check(ctx context.Context, list []Connection) []statusItem {
	out := make([]statusItem, len(list))
	var wg sync.WaitGroup
	for i, c := range list {
		s.mu.Lock()
		cached, ok := s.entries[c.ID]
		s.mu.Unlock()
		if ok && time.Since(cached.at) < 15*time.Second {
			out[i] = statusItem{ID: c.ID, Up: cached.up, Ms: cached.ms}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := probe(ctx, c.Hostname, c.Port)
			s.mu.Lock()
			s.entries[c.ID] = r
			s.mu.Unlock()
			out[i] = statusItem{ID: c.ID, Up: r.up, Ms: r.ms}
		}()
	}
	wg.Wait()
	return out
}
