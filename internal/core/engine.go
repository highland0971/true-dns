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
	"truedns/internal/probe"
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
	mu        sync.RWMutex
	cfg       *config.Config
	matcher   *matcher.Matcher
	cache     *cache.Cache
	doh       []*resolver.DoH
	system    *resolver.Plain
	systemFB  *resolver.Plain // public fallback chain (upstreams.fallback)
	overrideM *overrideManager
	prober    *probe.Prober // nil when probe.enabled = false
	sem       chan struct{}
	rot       atomic.Uint64 // failover rotation

	startedAt       time.Time
	queries         atomic.Uint64
	dohQueries      atomic.Uint64
	sysQueries      atomic.Uint64
	fbQueries       atomic.Uint64
	overrideQueries atomic.Uint64
	failures        atomic.Uint64
	lastFailure     atomic.Pointer[failure]
}

// New builds an Engine from cfg.
func New(cfg *config.Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m, doh, sys, fb, err := buildFrom(cfg)
	if err != nil {
		return nil, err
	}
	return &Engine{
		cfg:       cfg,
		matcher:   m,
		cache:     cache.New(cfg.Cache.MaxEntries, cfg.Cache.MaxTTL),
		doh:       doh,
		system:    sys,
		systemFB:  fb,
		overrideM: newOverrideManager(cfg),
		prober:    newProber(cfg),
		sem:       make(chan struct{}, maxInflight),
		startedAt: time.Now(),
	}, nil
}

// buildFrom constructs the derived components for a configuration without
// mutating anything, so reloads are atomic. The fallback chain excludes any
// address already present in the system upstreams.
func buildFrom(cfg *config.Config) (*matcher.Matcher, []*resolver.DoH, *resolver.Plain, *resolver.Plain, error) {
	m, err := matcher.New(cfg.Domains.Polluted)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("domains: %w", err)
	}
	doh := make([]*resolver.DoH, 0, len(cfg.Upstreams.DoH))
	for i, u := range cfg.Upstreams.DoH {
		d, err := resolver.NewDoH(fmt.Sprintf("doh-%d", i+1), u, cfg.Upstreams.Timeout, cfg.Upstreams.ProxyURL)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("upstream %q: %w", u, err)
		}
		doh = append(doh, d)
	}
	sysAddrs := cfg.Upstreams.System
	if len(sysAddrs) == 0 {
		sysAddrs = platform.Current().DiscoverSystemDNS()
	}
	sysUp := resolver.NewPlain(sysAddrs, cfg.Upstreams.Timeout)
	fbAddrs := make([]string, 0, len(cfg.Upstreams.Fallback))
	seen := map[string]bool{}
	for _, a := range sysAddrs {
		if na, err := resolver.WithPort(a); err == nil {
			seen[na] = true
		}
	}
	for _, a := range cfg.Upstreams.Fallback {
		na, err := resolver.WithPort(a)
		if err != nil || seen[na] {
			continue
		}
		seen[na] = true
		fbAddrs = append(fbAddrs, na)
	}
	var fbUp *resolver.Plain
	if len(fbAddrs) > 0 {
		fbUp = resolver.NewPlain(fbAddrs, cfg.Upstreams.Timeout)
	}
	return m, doh, sysUp, fbUp, nil
}

// Reload swaps in a new configuration and flushes cached entries so the new
// routing rules take effect immediately.
func (e *Engine) Reload(cfg *config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	m, doh, sys, fb, err := buildFrom(cfg)
	if err != nil {
		return err
	}
	om := newOverrideManager(cfg)
	e.mu.Lock()
	old := e.overrideM
	e.cfg, e.matcher, e.doh, e.system, e.systemFB, e.overrideM, e.prober = cfg, m, doh, sys, fb, om, newProber(cfg)
	e.mu.Unlock()
	old.shutdown()
	e.cache.Flush()
	return nil
}

// Shutdown stops background work (override refresh loop).
func (e *Engine) Shutdown() {
	e.mu.Lock()
	om := e.overrideM
	e.overrideM = nil
	e.mu.Unlock()
	if om != nil {
		om.shutdown()
	}
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

	// Hosts-format IP override table takes precedence over upstream routing
	// for A/AAAA queries on covered names.
	e.mu.RLock()
	om := e.overrideM
	cfg := e.cfg
	e.mu.RUnlock()
	if om != nil {
		if ips := om.lookup(q.Name); overrideHasFamily(ips, q.Qtype) {
			resp := synthesizeOverride(r, q, ips, cfg.Override.TTL)
			e.overrideQueries.Add(1)
			resp.Id = r.Id
			resp.Question = []dns.Question{q}
			finalizeEDNS(r, resp)
			if ttl := cache.TTLFromMsg(resp, cfg.Cache.MaxTTL); ttl > 0 {
				e.cache.Put(key, resp, ttl)
			}
			e.logQuery(r, resp, "override", time.Since(start))
			_ = w.WriteMsg(resp)
			return
		}
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
	cfg = e.cfg
	matched := e.matcher.Match(q.Name)
	viaDoH := cfg.Mode == config.ModeFull || matched
	dohUp := e.doh
	sysUp := e.system
	fbUp := e.systemFB
	pr := e.prober
	e.mu.RUnlock()

	out := prepareOutgoing(r, cfg, viaDoH)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Upstreams.Timeout)
	defer cancel()

	var (
		resp  *dns.Msg
		err   error
		route string
	)
	if viaDoH {
		e.dohQueries.Add(1)
		resp, err = e.exchangeDoH(ctx, out, cfg, dohUp)
		route = routeName(viaDoH)
	} else {
		route = "system"
		e.sysQueries.Add(1)
		resp, err = sysUp.Exchange(ctx, out)
		if err != nil && fbUp != nil {
			// Discovered system upstreams can be dead (e.g. VM host-only
			// gateways); give the configured public fallback chain its own
			// timeout budget. The route stays "system-fallback" even when
			// the fallback also fails, preserving the failure path in logs.
			route = "system-fallback"
			slog.Debug("system upstream failed, trying public fallback", "qname", q.Name, "err", err)
			fctx, fcancel := context.WithTimeout(context.Background(), cfg.Upstreams.Timeout)
			e.fbQueries.Add(1)
			resp, err = fbUp.Exchange(fctx, out)
			fcancel()
		}
		if err != nil && cfg.Upstreams.FallbackToDoH {
			slog.Warn("system upstreams failed, falling back to DoH", "qname", q.Name, "err", err)
			e.dohQueries.Add(1)
			resp, err = e.exchangeDoH(ctx, out, cfg, dohUp)
			if err == nil {
				route = "doh-fallback"
			}
		}
	}
	if err != nil {
		e.failures.Add(1)
		e.lastFailure.Store(&failure{Err: err.Error(), At: time.Now()})
		slog.Warn("resolve failed",
			"qname", q.Name, "qtype", dns.TypeToString[q.Qtype],
			"route", route, "err", err)
		e.replyRcode(w, r, dns.RcodeServerFailure)
		return
	}

	if pr != nil && matched && resp.Rcode == dns.RcodeSuccess {
		resp.Answer = filterAnswerIPs(resp.Answer, pr, cfg.Probe)
	}
	resp.Id = r.Id
	resp.Question = []dns.Question{q}
	finalizeEDNS(r, resp)
	if ttl := cache.TTLFromMsg(resp, cfg.Cache.MaxTTL); ttl > 0 {
		e.cache.Put(key, resp, ttl)
	}
	e.logQuery(r, resp, route, time.Since(start))
	_ = w.WriteMsg(resp)
}

func routeName(viaDoH bool) string {
	if viaDoH {
		return "doh"
	}
	return "system"
}

// overrideHasFamily reports whether the override IPs contain an address of
// the queried family; otherwise the query falls through to normal routing
// (a hosts entry with only A records must not suppress AAAA resolution).
func overrideHasFamily(ips []net.IP, qtype uint16) bool {
	for _, ip := range ips {
		switch qtype {
		case dns.TypeA:
			if ip.To4() != nil {
				return true
			}
		case dns.TypeAAAA:
			if ip.To4() == nil {
				return true
			}
		}
	}
	return false
}

// synthesizeOverride builds an A/AAAA answer directly from override-table IPs.
func synthesizeOverride(r *dns.Msg, q dns.Question, ips []net.IP, ttl time.Duration) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.RecursionAvailable = true
	ttl32 := uint32(ttl / time.Second)
	if ttl32 < 1 {
		ttl32 = 1
	}
	for _, ip := range ips {
		switch q.Qtype {
		case dns.TypeA:
			if v4 := ip.To4(); v4 != nil {
				m.Answer = append(m.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl32},
					A:   v4,
				})
			}
		case dns.TypeAAAA:
			if ip.To4() == nil {
				m.Answer = append(m.Answer, &dns.AAAA{
					Hdr:  dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl32},
					AAAA: ip.To16(),
				})
			}
		}
	}
	return m
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
	Version                 string          `json:"version"`
	Mode                    config.Mode     `json:"mode"`
	Listen                  []string        `json:"listen"`
	Strategy                config.Strategy `json:"strategy"`
	DoHUpstreams            []string        `json:"doh_upstreams"`
	SystemUpstreams         []string        `json:"system_upstreams"`
	SystemFallbackUpstreams []string        `json:"system_fallback_upstreams"`
	PollutedDomains         []string        `json:"polluted_domains"`
	CacheSize               int             `json:"cache_size"`
	CacheHits               uint64          `json:"cache_hits"`
	CacheMisses             uint64          `json:"cache_misses"`
	Queries                 uint64          `json:"queries"`
	DoHQueries              uint64          `json:"doh_queries"`
	SystemQueries           uint64          `json:"system_queries"`
	FallbackQueries         uint64          `json:"fallback_queries"`
	OverrideQueries         uint64          `json:"override_queries"`
	ProbeEnabled            bool            `json:"probe_enabled"`
	ProbeChecks             uint64          `json:"probe_checks"`
	OverrideEntries         int             `json:"override_entries"`
	OverrideUpdatedAt       time.Time       `json:"override_updated_at,omitempty"`
	OverrideLastError       string          `json:"override_last_error,omitempty"`
	Failures                uint64          `json:"failures"`
	LastFailure             *FailureInfo    `json:"last_failure,omitempty"`
	UptimeSeconds           int64           `json:"uptime_seconds"`
}

// StatsInterval returns the configured stats heartbeat interval (0 = off).
func (e *Engine) StatsInterval() time.Duration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg.Log.StatsInterval
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
	sysFB := []string{}
	if e.systemFB != nil {
		sysFB = append(sysFB, e.systemFB.Addrs()...)
	}
	domains := append([]string(nil), cfg.Domains.Polluted...)
	listen := append([]string(nil), cfg.Listen...)
	om := e.overrideM
	pr := e.prober
	e.mu.RUnlock()

	size, hits, misses := e.cache.Stats()
	s := Status{
		Version:                 version.Version,
		Mode:                    cfg.Mode,
		Listen:                  listen,
		Strategy:                cfg.Upstreams.Strategy,
		DoHUpstreams:            doh,
		SystemUpstreams:         sys,
		SystemFallbackUpstreams: sysFB,
		PollutedDomains:         domains,
		CacheSize:               size,
		CacheHits:               hits,
		CacheMisses:             misses,
		Queries:                 e.queries.Load(),
		DoHQueries:              e.dohQueries.Load(),
		SystemQueries:           e.sysQueries.Load(),
		FallbackQueries:         e.fbQueries.Load(),
		OverrideQueries:         e.overrideQueries.Load(),
		Failures:                e.failures.Load(),
		UptimeSeconds:           int64(time.Since(e.startedAt).Seconds()),
	}
	if pr != nil {
		s.ProbeEnabled = true
		s.ProbeChecks = pr.Stats()
	}
	if om != nil {
		meta := om.meta()
		s.OverrideEntries = meta.Entries
		s.OverrideUpdatedAt = meta.UpdatedAt
		s.OverrideLastError = meta.LastError
	}
	if f := e.lastFailure.Load(); f != nil {
		s.LastFailure = &FailureInfo{Error: f.Err, At: f.At}
	}
	return s
}
