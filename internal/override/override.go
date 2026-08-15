// Package override maintains a hostname→IP override table loaded from
// hosts-format files and URLs (e.g. the GitHub520 subscription), so curated
// IP lists can be applied without hosts-file surgery or administrator rights.
package override

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Meta describes the current table state for status reporting.
type Meta struct {
	Sources   []string  `json:"sources"`
	Entries   int       `json:"entries"`
	UpdatedAt time.Time `json:"updated_at"`
	LastError string    `json:"last_error,omitempty"`
}

// Table is safe for concurrent use.
type Table struct {
	mu   sync.RWMutex
	ips  map[string][]net.IP
	meta Meta
}

// New creates an empty table.
func New() *Table {
	return &Table{ips: make(map[string][]net.IP)}
}

// ParseHosts merges hosts-file content ("ip hostname [hostname...]", comments
// start with #) into the table and returns the number of hostnames added.
// Invalid lines are skipped.
func (t *Table) ParseHosts(r io.Reader) (int, error) {
	added := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil {
			continue
		}
		for _, name := range fields[1:] {
			name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
			if name == "" {
				continue
			}
			t.mu.Lock()
			prev := t.ips[name]
			dup := false
			for _, p := range prev {
				if p.Equal(ip) {
					dup = true
					break
				}
			}
			if !dup {
				t.ips[name] = append(prev, ip)
				added++
			}
			t.mu.Unlock()
		}
	}
	return added, sc.Err()
}

// Lookup returns the override IPs for name (nil when not covered).
func (t *Table) Lookup(name string) []net.IP {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]net.IP(nil), t.ips[name]...)
}

// Meta returns the current table metadata.
func (t *Table) Meta() Meta {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.meta
}

// LoadFiles parses each hosts-format file into the table.
func (t *Table) LoadFiles(paths []string) error {
	var lastErr error
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			lastErr = fmt.Errorf("override file %s: %w", p, err)
			continue
		}
		added, err := t.ParseHosts(f)
		f.Close()
		if err != nil {
			lastErr = fmt.Errorf("override file %s: %w", p, err)
			continue
		}
		t.recordSource(p, added, nil)
	}
	return lastErr
}

// LoadURLs fetches each hosts-format URL into the table. proxyURL reuses the
// optional DoH proxy for fetching (a GitHub-hosted list resolves through the
// proxy itself once the system DNS has been taken over).
func (t *Table) LoadURLs(ctx context.Context, urls []string, proxyURL string, client *http.Client) error {
	tr := client.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	if _, ok := tr.(*http.Transport); ok && proxyURL != "" {
		if pu, err := url.Parse(proxyURL); err == nil {
			t2 := tr.(*http.Transport).Clone()
			t2.Proxy = http.ProxyURL(pu)
			tr = t2
		}
	}
	c := *client
	c.Transport = tr
	var lastErr error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = fmt.Errorf("override url %s: %w", u, err)
			continue
		}
		req.Header.Set("User-Agent", "truedns-override")
		resp, err := c.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("override url %s: %w", u, err)
			continue
		}
		added, err := t.ParseHosts(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("override url %s: %w", u, err)
			continue
		}
		t.recordSource(u, added, nil)
	}
	if lastErr != nil {
		t.recordError(lastErr)
	}
	return lastErr
}

func (t *Table) recordSource(src string, added int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.meta.Sources = append(t.meta.Sources, src)
	t.meta.Entries = len(t.ips)
	t.meta.UpdatedAt = time.Now()
	if err != nil {
		t.meta.LastError = err.Error()
	} else {
		t.meta.LastError = ""
	}
	_ = added
}

func (t *Table) recordError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.meta.LastError = err.Error()
}
