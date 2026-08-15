// Package cache implements an in-memory, TTL-based DNS response cache with
// LRU eviction, safe for concurrent use.
package cache

import (
	"container/list"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

type entry struct {
	key     string
	msg     *dns.Msg
	expires time.Time
}

// Cache is an LRU DNS response cache with per-entry expiry.
type Cache struct {
	mu     sync.Mutex
	lru    *list.List
	index  map[string]*list.Element
	max    int
	maxTTL time.Duration
	hits   uint64
	misses uint64
}

// New creates a cache holding at most maxEntries entries, capping stored TTLs
// at maxTTL.
func New(maxEntries int, maxTTL time.Duration) *Cache {
	return &Cache{
		lru:    list.New(),
		index:  make(map[string]*list.Element),
		max:    maxEntries,
		maxTTL: maxTTL,
	}
}

// Key builds the canonical cache key for a question.
func Key(q dns.Question) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(q.Name))
	b.WriteByte('|')
	b.WriteString(dns.TypeToString[q.Qtype])
	b.WriteByte('|')
	b.WriteString(dns.ClassToString[q.Qclass])
	return b.String()
}

// Get returns a cached response for the key. TTLs in the returned message are
// decremented to reflect the remaining lifetime.
func (c *Cache) Get(key string) (*dns.Msg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		c.misses++
		return nil, false
	}
	e := el.Value.(*entry)
	remaining := time.Until(e.expires)
	if remaining <= 0 {
		c.removeLocked(el)
		c.misses++
		return nil, false
	}
	c.hits++
	c.lru.MoveToFront(el)
	msg := e.msg.Copy()
	ttl := uint32((remaining + time.Second - 1) / time.Second) // ceil
	if ttl < 1 {
		ttl = 1
	}
	setTTLs(msg, ttl)
	return msg, true
}

// Put stores a response under key. ttl <= 0 skips caching; ttl is capped at
// the cache's maxTTL.
func (c *Cache) Put(key string, msg *dns.Msg, ttl time.Duration) {
	if ttl <= 0 || msg == nil {
		return
	}
	if ttl > c.maxTTL {
		ttl = c.maxTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		e := el.Value.(*entry)
		e.msg, e.expires = msg.Copy(), time.Now().Add(ttl)
		c.lru.MoveToFront(el)
		return
	}
	// Evict expired entries first, then the LRU tail if still over capacity.
	for el := c.lru.Back(); el != nil && c.lru.Len() >= c.max; el = c.lru.Back() {
		e := el.Value.(*entry)
		if !time.Now().After(e.expires) {
			break
		}
		c.removeLocked(el)
	}
	if c.lru.Len() >= c.max {
		c.removeLocked(c.lru.Back())
	}
	el := c.lru.PushFront(&entry{key: key, msg: msg.Copy(), expires: time.Now().Add(ttl)})
	c.index[key] = el
}

// Flush drops all entries.
func (c *Cache) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.Init()
	c.index = make(map[string]*list.Element)
}

// Stats reports the current entry count and cumulative hit/miss counters.
func (c *Cache) Stats() (size int, hits, misses uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.hits, c.misses
}

func (c *Cache) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	delete(c.index, e.key)
	c.lru.Remove(el)
}

// setTTLs rewrites the TTL of every non-OPT record in the answer and
// authority sections.
func setTTLs(msg *dns.Msg, ttl uint32) {
	for _, rr := range msg.Answer {
		if rr.Header().Rrtype != dns.TypeOPT {
			rr.Header().Ttl = ttl
		}
	}
	for _, rr := range msg.Ns {
		if rr.Header().Rrtype != dns.TypeOPT {
			rr.Header().Ttl = ttl
		}
	}
}

// TTLFromMsg computes the effective cache TTL of a response: the minimum TTL
// across answer and authority records, capped at maxTTL. Responses without any
// TTL-bearing records (e.g. SERVFAIL/REFUSED) and truncated responses are not
// cacheable. Negative answers carry the SOA TTL in the authority section.
func TTLFromMsg(msg *dns.Msg, maxTTL time.Duration) time.Duration {
	if msg == nil || msg.Truncated {
		return 0
	}
	ttl := time.Duration(1<<63 - 1)
	found := false
	for _, sec := range [][]dns.RR{msg.Answer, msg.Ns} {
		for _, rr := range sec {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			found = true
			t := time.Duration(rr.Header().Ttl) * time.Second
			// RFC 2308: the negative-cache TTL of an SOA is the smaller of
			// its own TTL and the MINIMUM field.
			if soa, ok := rr.(*dns.SOA); ok {
				if m := time.Duration(soa.Minttl) * time.Second; m < t {
					t = m
				}
			}
			if t < ttl {
				ttl = t
			}
		}
	}
	if !found {
		return 0
	}
	if ttl > maxTTL {
		ttl = maxTTL
	}
	if ttl <= 0 {
		return 0
	}
	return ttl
}
