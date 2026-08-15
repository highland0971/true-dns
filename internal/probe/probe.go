// Package probe checks TCP reachability of candidate IPs (GitHub520-style
// port-443 probing) so the engine can drop or reorder addresses of
// poisoned-domain answers.
//
// Latency budget: Filter dials candidates concurrently with a small worker
// cap. The engine filters the A and AAAA families in series, so the
// worst-case added latency is
// 2 * ceil(max_ips / probeConcurrency) * timeout.
package probe

import (
	"net"
	"sort"
	"sync"
	"time"
)

// probeConcurrency caps concurrent dials inside one Filter call.
const probeConcurrency = 4

// Mode selects how probe results reshape an answer set.
type Mode string

const (
	// ModeDrop removes unreachable IPs (keeping the original set when every
	// candidate is unreachable, so transient failures never empty an answer).
	ModeDrop Mode = "drop"
	// ModePrefer sorts reachable IPs first by latency without dropping.
	ModePrefer Mode = "prefer"
)

// Result is the outcome of one reachability check.
type Result struct {
	Reachable bool
	Latency   time.Duration
}

type cacheEntry struct {
	res      Result
	expires  time.Time
	inserted time.Time
}

// cacheMaxEntries bounds the result cache; the oldest entry is evicted when
// full (expired entries are swept first).
const cacheMaxEntries = 4096

type inflightEntry struct {
	done chan struct{}
	res  Result
}

// Prober is safe for concurrent use.
type Prober struct {
	mu       sync.Mutex
	cache    map[string]cacheEntry
	inflight map[string]*inflightEntry // per-IP dial dedup (singleflight)
	port     int
	timeout  time.Duration
	ttl      time.Duration
	checks   uint64 // actual dials performed
}

// New creates a Prober dialing port with timeout; results are cached for
// cacheTTL (0 disables caching).
func New(port int, timeout, cacheTTL time.Duration) *Prober {
	return &Prober{
		cache:    make(map[string]cacheEntry),
		inflight: make(map[string]*inflightEntry),
		port:     port,
		timeout:  timeout,
		ttl:      cacheTTL,
	}
}

// Check probes ip once (cached within the TTL window; concurrent callers of
// the same cache miss share a single dial).
func (p *Prober) Check(ip net.IP) Result {
	key := ip.String()
	now := time.Now()
	p.mu.Lock()
	if e, ok := p.cache[key]; ok && now.Before(e.expires) {
		p.mu.Unlock()
		return e.res
	}
	if in, ok := p.inflight[key]; ok {
		p.mu.Unlock()
		<-in.done
		return in.res
	}
	in := &inflightEntry{done: make(chan struct{})}
	p.inflight[key] = in
	p.mu.Unlock()

	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), itoa(p.port)), p.timeout)
	lat := time.Since(start)
	res := Result{Latency: lat}
	if err == nil {
		conn.Close()
		res.Reachable = true
	}

	p.mu.Lock()
	p.checks++
	if p.ttl > 0 {
		if _, exists := p.cache[key]; !exists && len(p.cache) >= cacheMaxEntries {
			p.sweepExpiredLocked(now)
			if len(p.cache) >= cacheMaxEntries {
				p.evictOldestLocked()
			}
		}
		p.cache[key] = cacheEntry{res: res, expires: now.Add(p.ttl), inserted: now}
	}
	in.res = res
	close(in.done)
	delete(p.inflight, key)
	p.mu.Unlock()
	return res
}

// Stats reports how many actual dials have been performed.
func (p *Prober) Stats() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checks
}

// Filter reshapes ips per mode: drop removes unreachable addresses (falling
// back to the original set when none survive); prefer sorts reachable first
// by latency. At most max IPs are probed (the rest keep their positions).
// Dials run concurrently, capped at probeConcurrency.
func (p *Prober) Filter(ips []net.IP, mode Mode, max int) []net.IP {
	if len(ips) <= 1 {
		return ips
	}
	limit := max
	if limit <= 0 || limit > len(ips) {
		limit = len(ips)
	}
	type probed struct {
		ip  net.IP
		res Result
	}
	ps := make([]probed, limit)
	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ps[i].ip = ips[i]
			ps[i].res = p.Check(ips[i])
		}(i)
	}
	wg.Wait()

	reachable := func(pp []probed) []net.IP {
		out := make([]net.IP, 0, len(pp))
		for _, x := range pp {
			if x.res.Reachable {
				out = append(out, x.ip)
			}
		}
		return out
	}
	switch mode {
	case ModeDrop:
		kept := reachable(ps)
		if len(kept) == 0 {
			return ips // all unreachable: keep the original answer
		}
		out := append(kept, ips[limit:]...)
		return out
	default: // prefer
		stable := append([]probed(nil), ps...)
		sort.SliceStable(stable, func(i, j int) bool {
			if stable[i].res.Reachable != stable[j].res.Reachable {
				return stable[i].res.Reachable
			}
			if stable[i].res.Reachable {
				return stable[i].res.Latency < stable[j].res.Latency
			}
			return false
		})
		out := make([]net.IP, 0, len(ips))
		for _, x := range stable {
			out = append(out, x.ip)
		}
		out = append(out, ips[limit:]...)
		return out
	}
}

// sweepExpiredLocked removes entries past their TTL; caller holds the lock.
func (p *Prober) sweepExpiredLocked(now time.Time) {
	for k, e := range p.cache {
		if !now.Before(e.expires) {
			delete(p.cache, k)
		}
	}
}

// evictOldestLocked removes the least recently inserted entry; caller holds
// the lock and the cache must be non-empty.
func (p *Prober) evictOldestLocked() {
	var oldestK string
	var oldestT time.Time
	first := true
	for k, e := range p.cache {
		if first || e.inserted.Before(oldestT) {
			oldestK, oldestT, first = k, e.inserted, false
		}
	}
	delete(p.cache, oldestK)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
