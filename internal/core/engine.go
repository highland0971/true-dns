// Package core wires DNS listeners, routing, cache and upstreams into the
// true-dns engine. It is platform-independent: OS-specific behavior (system
// DNS takeover, upstream discovery) lives in internal/platform.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"truedns/internal/cache"
	"truedns/internal/config"
	"truedns/internal/matcher"
	"truedns/internal/platform"
	"truedns/internal/resolver"
	"truedns/internal/version"
)

// maxInflight caps concurrently forwarded upstream queries. Beyond the cap we
// answer SERVFAIL immediately; DNS clients retry.
const maxInflight = 512

type failure struct {
	Err string    `json:"error"`
	At  time.Time `json:"at"`
}

// Engine routes and answers DNS queries. It is safe for concurrent use.
type Engine struct {
	mu      sync.RWMutex
	cfg     *config.Config
	matcher *matcher.Matcher
	cache   *cache.Cache
	doh     []*resolver.DoH
	system  *resolver.Plain
	sem     chan struct{}
	rot     atomic.Uint64 // failover rotation

	startedAt   time.Time
	queries     atomic.Uint64
	dohQueries  atomic.Uint64
	sysQueries  atomic.Uint64
	failures    atomic.Uint64
	lastFailure atomic.Pointer[failure]
}

// New builds an Engine from cfg.
func New(cfg *config.Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m, doh, sys, err := buildFrom(cfg)
	if err != nil {
		return nil, err
	}
	return &Engine{
		cfg:       cfg,
		matcher:   m,
		cache:     cache.New(cfg.Cache.MaxEntries, cfg.Cache.MaxTTL),
		doh:       doh,
		system:    sys,
		sem:       make(chan struct{}, maxInflight),
		startedAt: time.Now(),
	}, nil
}

// buildFrom constructs the derived components for a configuration without
// mutating anything, so reloads are atomic.
func buildFrom(cfg *config.Config) (*matcher.Matcher, []*resolver.DoH, *resolver.Plain, error) {
	m, err := matcher.New(cfg.Domains.Polluted)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("domains: %w", err)
	}
	doh := make([]*resolver.DoH, 0, len(cfg.Upstreams.DoH))
	for i, u := range cfg.Upstreams.DoH {
		d, err := resolver.NewDoH(fmt.Sprintf("doh-%d", i+1), u, cfg.Upstreams.Timeout, cfg.Upstreams.ProxyURL)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("upstream %q: %w", u, err)
		}
		doh = append(doh, d)
	}
	sysAddrs := cfg.Upstreams.System
	if len(sysAddrs) == 0 {
		sysAddrs = platform.Current().DiscoverSystemDNS()
	}
	return m, doh, resolver.NewPlain(sysAddrs, cfg.Upstreams.Timeout), nil
}

// Reload swaps in a new configuration and flushes cached entries so the new
// routing rules take effect immediately.
func (e *Engine) Reload(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	m, doh, sys, err := buildFrom(cfg)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg, e.matcher, e.doh, e.system = cfg, m, doh, sys
	e.cache.Flush()
	return nil
}

// ServeDNS implements dns.Handler.
func (e *Engine) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	start := time.Now()
	e.queries.Add(1)

	if len(r.Question) != 1 {
		e.replyRcode(w, r, dns.RcodeFormatError)
		return
	}
	q := r.Question[0]
	key := cache.Key(q)

	if hit, ok := e.cache.Get(key); ok {
		hit.Id = r.Id
		hit.Question = []dns.Question{q}
		finalizeEDNS(r, hit)
		e.logQuery(r, hit, "cache", time.Since(start))
		_ = w.WriteMsg(hit)
		return
	}

	// Concurrency limit: answer SERVFAIL when saturated; clients retry.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	default:
		e.replyRcode(w, r, dns.RcodeServerFailure)
		return
	}

	e.mu.RLock()
	cfg := e.cfg
	viaDoH := cfg.Mode == config.ModeFull || e.matcher.Match(q.Name)
	dohUp := e.doh
	sysUp := e.system
	e.mu.RUnlock()

	out := prepareOutgoing(r, cfg, viaDoH)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Upstreams.Timeout)
	defer cancel()

	var (
		resp *dns.Msg
		err  error
	)
	if viaDoH {
		e.dohQueries.Add(1)
		resp, err = e.exchangeDoH(ctx, out, cfg, dohUp)
	} else {
		e.sysQueries.Add(1)
		resp, err = sysUp.Exchange(ctx, out)
		if err != nil && cfg.Upstreams.FallbackToDoH {
			slog.Warn("system upstream failed, falling back to DoH", "qname", q.Name, "err", err)
			e.dohQueries.Add(1)
			resp, err = e.exchangeDoH(ctx, out, cfg, dohUp)
		}
	}
	if err != nil {
		e.failures.Add(1)
		e.lastFailure.Store(&failure{Err: err.Error(), At: time.Now()})
		slog.Debug("resolve failed",
			"qname", q.Name, "qtype", dns.TypeToString[q.Qtype],
			"route", routeName(viaDoH), "err", err)
		e.replyRcode(w, r, dns.RcodeServerFailure)
		return
	}

	resp.Id = r.Id
	resp.Question = []dns.Question{q}
	finalizeEDNS(r, resp)
	if ttl := cache.TTLFromMsg(resp, cfg.Cache.MaxTTL); ttl > 0 {
		e.cache.Put(key, resp, ttl)
	}
	e.logQuery(r, resp, routeName(viaDoH), time.Since(start))
	_ = w.WriteMsg(resp)
}

func routeName(viaDoH bool) string {
	if viaDoH {
		return "doh"
	}
	return "system"
}

func (e *Engine) replyRcode(w dns.ResponseWriter, r *dns.Msg, code int) {
	m := new(dns.Msg)
	m.SetRcode(r, code)
	_ = w.WriteMsg(m)
}

func (e *Engine) logQuery(r, resp *dns.Msg, route string, dur time.Duration) {
	e.mu.RLock()
	verbose := e.cfg.Log.VerboseQueries
	e.mu.RUnlock()
	if !verbose {
		return
	}
	q := r.Question[0]
	slog.Info("query",
		"name", q.Name,
		"type", dns.TypeToString[q.Qtype],
		"rcode", dns.RcodeToString[resp.Rcode],
		"answers", len(resp.Answer),
		"route", route,
		"dur_ms", dur.Milliseconds(),
	)
}

// exchangeDoH forwards a query to the DoH upstreams according to strategy.
func (e *Engine) exchangeDoH(ctx context.Context, req *dns.Msg, cfg *config.Config, ups []*resolver.DoH) (*dns.Msg, error) {
	if len(ups) == 0 {
		return nil, errors.New("no DoH upstreams configured")
	}
	switch cfg.Upstreams.Strategy {
	case config.StrategyRace:
		results := make(chan *dns.Msg, len(ups))
		var wg sync.WaitGroup
		for _, u := range ups {
			wg.Add(1)
			go func(u *resolver.DoH) {
				defer wg.Done()
				if resp, err := u.Exchange(ctx, req); err == nil {
					select {
					case results <- resp:
					default:
					}
				}
			}(u)
		}
		go func() { wg.Wait(); close(results) }()
		if resp := <-results; resp != nil {
			return resp, nil
		}
		return nil, errors.New("all DoH upstreams failed")
	default: // failover
		var lastErr error
		start := int(e.rot.Add(1)-1) % len(ups)
		for i := range ups {
			u := ups[(start+i)%len(ups)]
			resp, err := u.Exchange(ctx, req)
			if err == nil {
				return resp, nil
			}
			lastErr = err
		}
		return nil, fmt.Errorf("all DoH upstreams failed: %w", lastErr)
	}
}

// prepareOutgoing builds the upstream query: a fresh single-question message
// carrying the client's EDNS state, with ECS stripped or spoofed on the DoH
// path per configuration.
func prepareOutgoing(r *dns.Msg, cfg *config.Config, viaDoH bool) *dns.Msg {
	out := new(dns.Msg)
	out.Id = r.Id
	out.RecursionDesired = true
	out.CheckingDisabled = r.CheckingDisabled
	out.Question = []dns.Question{r.Question[0]}
	opt := r.IsEdns0()
	if opt == nil {
		return out
	}
	if !viaDoH {
		// System upstream: forward the client's OPT as-is (local resolver,
		// no cross-border privacy concern).
		out.Extra = append(out.Extra, opt)
		return out
	}
	// DoH upstream: rebuild the OPT per the ECS policy. Default: strip ECS so
	// the client's subnet is not leaked and CDN geo does not follow it.
	out.SetEdns0(4096, false)
	ne := out.IsEdns0()
	if opt.Do() {
		ne.SetDo()
	}
	switch {
	case cfg.ECS.Spoof != "":
		if sub, err := subnetOption(cfg.ECS.Spoof); err == nil {
			ne.Option = append(ne.Option, sub)
		}
	case !cfg.ECS.Strip:
		ne.Option = append(ne.Option, opt.Option...)
	}
	return out
}

// finalizeEDNS ensures the response carries an OPT record when the client
// sent one and the upstream reply did not include one.
func finalizeEDNS(req, resp *dns.Msg) {
	if req.IsEdns0() != nil && resp.IsEdns0() == nil {
		resp.SetEdns0(4096, false)
	}
}

// subnetOption builds an EDNS Client Subnet option from a CIDR string.
func subnetOption(cidr string) (*dns.EDNS0_SUBNET, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ones, _ := ipnet.Mask.Size()
	family := uint16(1)
	if ip.To4() != nil {
		ip = ip.To4()
	} else {
		family = 2
		ip = ip.To16()
	}
	return &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        family,
		SourceNetmask: uint8(ones),
		SourceScope:   0,
		Address:       ip,
	}, nil
}

// FlushCache drops all cached entries.
func (e *Engine) FlushCache() { e.cache.Flush() }

// FailureInfo describes the most recent upstream failure.
type FailureInfo struct {
	Error string    `json:"error"`
	At    time.Time `json:"at"`
}

// Status describes the engine for the control API and CLI status command.
type Status struct {
	Version         string          `json:"version"`
	Mode            config.Mode     `json:"mode"`
	Listen          []string        `json:"listen"`
	Strategy        config.Strategy `json:"strategy"`
	DoHUpstreams    []string        `json:"doh_upstreams"`
	SystemUpstreams []string        `json:"system_upstreams"`
	PollutedDomains []string        `json:"polluted_domains"`
	CacheSize       int             `json:"cache_size"`
	CacheHits       uint64          `json:"cache_hits"`
	CacheMisses     uint64          `json:"cache_misses"`
	Queries         uint64          `json:"queries"`
	DoHQueries      uint64          `json:"doh_queries"`
	SystemQueries   uint64          `json:"system_queries"`
	Failures        uint64          `json:"failures"`
	LastFailure     *FailureInfo    `json:"last_failure,omitempty"`
	UptimeSeconds   int64           `json:"uptime_seconds"`
}

// Status snapshots the engine state.
func (e *Engine) Status() Status {
	e.mu.RLock()
	cfg := e.cfg
	doh := make([]string, len(e.doh))
	for i, u := range e.doh {
		doh[i] = u.URL.String()
	}
	sys := append([]string(nil), e.system.Addrs()...)
	domains := append([]string(nil), cfg.Domains.Polluted...)
	listen := append([]string(nil), cfg.Listen...)
	e.mu.RUnlock()

	size, hits, misses := e.cache.Stats()
	s := Status{
		Version:         version.Version,
		Mode:            cfg.Mode,
		Listen:          listen,
		Strategy:        cfg.Upstreams.Strategy,
		DoHUpstreams:    doh,
		SystemUpstreams: sys,
		PollutedDomains: domains,
		CacheSize:       size,
		CacheHits:       hits,
		CacheMisses:     misses,
		Queries:         e.queries.Load(),
		DoHQueries:      e.dohQueries.Load(),
		SystemQueries:   e.sysQueries.Load(),
		Failures:        e.failures.Load(),
		UptimeSeconds:   int64(time.Since(e.startedAt).Seconds()),
	}
	if f := e.lastFailure.Load(); f != nil {
		s.LastFailure = &FailureInfo{Error: f.Err, At: f.At}
	}
	return s
}
