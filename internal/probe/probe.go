// Package probe checks TCP reachability of candidate IPs (GitHub520-style
// port-443 probing) so the engine can drop or reorder addresses of
// poisoned-domain answers.
package probe

import (
	"net"
	"sort"
	"sync"
	"time"
)

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
	res     Result
	expires time.Time
}

// Prober is safe for concurrent use.
type Prober struct {
	mu      sync.Mutex
	cache   map[string]cacheEntry
	port    int
	timeout time.Duration
	ttl     time.Duration
	checks  uint64 // cache-miss dials performed
}

// New creates a Prober dialing port with timeout; results are cached for
// cacheTTL (0 disables caching).
func New(port int, timeout, cacheTTL time.Duration) *Prober {
	return &Prober{
		cache:   make(map[string]cacheEntry),
		port:    port,
		timeout: timeout,
		ttl:     cacheTTL,
	}
}

// Check probes ip once (cached within the TTL window) and returns the result.
func (p *Prober) Check(ip net.IP) Result {
	key := ip.String()
	now := time.Now()
	p.mu.Lock()
	if e, ok := p.cache[key]; ok && now.Before(e.expires) {
		p.mu.Unlock()
		return e.res
	}
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
		p.cache[key] = cacheEntry{res: res, expires: now.Add(p.ttl)}
	}
	p.mu.Unlock()
	return res
}

// Stats reports how many cache-miss probes have been performed.
func (p *Prober) Stats() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checks
}

// Filter reshapes ips per mode: drop removes unreachable addresses (falling
// back to the original set when none survive); prefer sorts reachable first
// by latency. At most max IPs are probed (the rest keep their positions).
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
	ps := make([]probed, 0, limit)
	for i := 0; i < limit; i++ {
		ps = append(ps, probed{ip: ips[i], res: p.Check(ips[i])})
	}
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
