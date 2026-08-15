// Package matcher implements suffix-based domain matching for the polluted
// domain list.
//
// Semantics:
//   - A plain entry "github.com" matches github.com and every subdomain
//     (i.e. it behaves like "github.com" + "*github.com").
//   - A wildcard entry "*.github.com" matches strict subdomains only.
//   - "*" matches everything.
//
// Matching is O(number of labels) per query with map lookups.
package matcher

import (
	"fmt"
	"strings"
	"sync"
)

// Matcher is safe for concurrent use.
type Matcher struct {
	mu       sync.RWMutex
	roots    map[string]bool // plain entries: root and all subdomains
	wildcard map[string]bool // "*.root" entries: strict subdomains only
	matchAll bool
}

// New builds a Matcher from the given patterns.
func New(patterns []string) (*Matcher, error) {
	m := &Matcher{}
	if err := m.Reset(patterns); err != nil {
		return nil, err
	}
	return m, nil
}

// Reset atomically replaces the pattern set.
func (m *Matcher) Reset(patterns []string) error {
	roots := make(map[string]bool)
	wildcard := make(map[string]bool)
	matchAll := false
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == "*" {
			matchAll = true
			continue
		}
		p = strings.TrimSuffix(p, ".")
		if strings.HasPrefix(p, "*.") {
			root := strings.TrimPrefix(p, "*.")
			if root == "" || strings.ContainsAny(root, "*") {
				return fmt.Errorf("invalid domain pattern %q", p)
			}
			wildcard[root] = true
			continue
		}
		if strings.ContainsAny(p, "*") {
			return fmt.Errorf("invalid domain pattern %q: wildcards are only allowed as a leading \"*.\"", p)
		}
		roots[p] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roots, m.wildcard, m.matchAll = roots, wildcard, matchAll
	return nil
}

// Match reports whether name matches any configured pattern.
func (m *Matcher) Match(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.matchAll {
		return true
	}
	if m.roots[name] {
		return true
	}
	for {
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return false
		}
		name = name[i+1:]
		if m.roots[name] || m.wildcard[name] {
			return true
		}
	}
}

// Len returns the number of configured patterns.
func (m *Matcher) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := len(m.roots) + len(m.wildcard)
	if m.matchAll {
		n++
	}
	return n
}
