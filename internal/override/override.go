// Package override maintains a hostname→IP override table loaded from
// hosts-format files and URLs (e.g. the GitHub520 subscription), so curated
// IP lists can be applied without hosts-file surgery or administrator rights.
//
// Entries are tracked per source: re-loading a source REPLACES its previous
// entries, so periodic refreshes correctly drop IPs that disappeared from the
// upstream list.
package override

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
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
	mu      sync.RWMutex
	sources map[string]map[string][]net.IP // source → hostname → IPs
	meta    Meta
}

// New creates an empty table.
func New() *Table {
	return &Table{sources: make(map[string]map[string][]net.IP)}
}

// loadSource parses hosts-file content ("ip hostname [hostname...]",
// comments start with #) into a fresh source map, replacing any previous
// entries under src. It returns the number of hostnames stored.
func (t *Table) loadSource(src string, r io.Reader) (int, error) {
	parsed := make(map[string][]net.IP)
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
			if !containsIP(parsed[name], ip) {
				parsed[name] = append(parsed[name], ip)
			}
		}
	}
	t.mu.Lock()
	t.sources[src] = parsed
	t.refreshMetaLocked(sc.Err())
	t.mu.Unlock()
	return len(parsed), sc.Err()
}

// refreshMetaLocked rebuilds the derived metadata; caller holds the lock.
func (t *Table) refreshMetaLocked(lastErr error) {
	srcs := make([]string, 0, len(t.sources))
	total := 0
	for s, m := range t.sources {
		srcs = append(srcs, s)
		total += len(m)
	}
	sort.Strings(srcs)
	t.meta.Sources = srcs
	t.meta.Entries = total
	t.meta.UpdatedAt = time.Now()
	if lastErr != nil {
		t.meta.LastError = lastErr.Error()
	} else {
		t.meta.LastError = ""
	}
}

func containsIP(ips []net.IP, ip net.IP) bool {
	for _, p := range ips {
		if p.Equal(ip) {
			return true
		}
	}
	return false
}

// Lookup returns the override IPs for name (nil when not covered), merged
// across sources and deduplicated.
func (t *Table) Lookup(name string) []net.IP {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []net.IP
	for _, m := range t.sources {
		for _, ip := range m[name] {
			if !containsIP(out, ip) {
				out = append(out, ip)
			}
		}
	}
	return out
}

// Meta returns the current table metadata.
func (t *Table) Meta() Meta {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.meta
}

// LoadFiles parses each hosts-format file into the table (per-file replace
// semantics). A missing file keeps that source's previous entries.
func (t *Table) LoadFiles(paths []string) error {
	var lastErr error
	for _, p := range paths {
		f, err := openFile(p)
		if err != nil {
			lastErr = fmt.Errorf("override file %s: %w", p, err)
			t.recordError(lastErr)
			continue
		}
		_, err = t.loadSource(p, f)
		f.Close()
		if err != nil {
			lastErr = fmt.Errorf("override file %s: %w", p, err)
		}
	}
	return lastErr
}

// LoadURLs fetches each hosts-format URL into the table (per-URL replace
// semantics; a failed fetch keeps that URL's previous entries). proxyURL
// reuses the optional DoH proxy for fetching.
func (t *Table) LoadURLs(ctx context.Context, urls []string, proxyURL string, client *http.Client) error {
	tr := client.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	if ht, ok := tr.(*http.Transport); ok && proxyURL != "" {
		if pu, err := url.Parse(proxyURL); err == nil {
			cloned := ht.Clone()
			cloned.Proxy = http.ProxyURL(pu)
			tr = cloned
		}
	}
	c := *client
	c.Transport = tr
	var lastErr error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = fmt.Errorf("override url %s: %w", u, err)
			t.recordError(lastErr)
			continue
		}
		req.Header.Set("User-Agent", "truedns-override")
		resp, err := c.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("override url %s: %w", u, err)
			t.recordError(lastErr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("override url %s: http %d", u, resp.StatusCode)
			t.recordError(lastErr)
			continue
		}
		_, perr := t.loadSource(u, io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if perr != nil {
			lastErr = fmt.Errorf("override url %s: %w", u, perr)
		}
	}
	return lastErr
}

func (t *Table) recordError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.meta.LastError = err.Error()
}

// openFile is a variable so tests can substitute a failing opener.
var openFile = func(path string) (io.ReadCloser, error) {
	return osOpen(path)
}
