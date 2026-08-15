package core

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"truedns/internal/config"
)

// testWriter is a minimal dns.ResponseWriter capturing the response message.
type testWriter struct{ m *dns.Msg }

func (w *testWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53000}
}
func (w *testWriter) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53001}
}
func (w *testWriter) WriteMsg(m *dns.Msg) error {
	w.m = m
	return nil
}
func (w *testWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *testWriter) Close() error                { return nil }
func (w *testWriter) TsigStatus() error           { return nil }
func (w *testWriter) TsigTimersOnly(bool)         {}
func (w *testWriter) Hijack()                     {}

// fakeDoH is an httptest-backed DoH upstream with a query counter.
type fakeDoH struct {
	queries  atomic.Int64
	check    func(*dns.Msg) // optional request inspection
	answer   string
	ttl      uint32
	rcode    int
	failHTTP bool
}

func (f *fakeDoH) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.queries.Add(1)
		buf := make([]byte, 65535)
		n, _ := r.Body.Read(buf)
		req := new(dns.Msg)
		if err := req.Unpack(buf[:n]); err != nil {
			http.Error(w, "unpack", http.StatusBadRequest)
			return
		}
		if f.failHTTP {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if f.check != nil {
			f.check(req)
		}
		m := new(dns.Msg)
		m.SetRcode(req, f.rcode)
		if f.rcode == dns.RcodeSuccess {
			m.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: f.ttl},
				A:   net.ParseIP(f.answer).To4(),
			}}
		} else if f.rcode == dns.RcodeNameError {
			m.Ns = []dns.RR{&dns.SOA{
				Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
				Ns:  "ns.example.", Mbox: "host.example.",
				Serial: 1, Refresh: 2, Retry: 3, Expire: 4, Minttl: 60,
			}}
		}
		wire, _ := m.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(wire)
	}
}

// startUDPDNS runs a plain UDP DNS server answering every A query with answer.
func startUDPDNS(t *testing.T, answer string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP(answer).To4(),
		}}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return addr
}

func testConfig(mode config.Mode, dohURL, sysAddr string, polluted ...string) *config.Config {
	cfg := config.Default()
	cfg.Mode = mode
	cfg.Listen = []string{"127.0.0.1:0"} // unused in unit tests
	cfg.Upstreams.DoH = []string{dohURL}
	cfg.Upstreams.System = []string{sysAddr}
	cfg.Upstreams.Timeout = 2 * time.Second
	cfg.Domains.Polluted = polluted
	cfg.API.Enabled = false
	return cfg
}

func query(t *testing.T, e *Engine, name string, qtype uint16, edns bool) *dns.Msg {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(name), qtype)
	if edns {
		req.SetEdns0(4096, false)
	}
	w := &testWriter{}
	e.ServeDNS(w, req)
	if w.m == nil {
		t.Fatal("no response written")
	}
	return w.m
}

func firstA(t *testing.T, resp *dns.Msg) string {
	t.Helper()
	if len(resp.Answer) != 1 {
		t.Fatalf("want 1 answer, got %d (rcode %s)", len(resp.Answer), dns.RcodeToString[resp.Rcode])
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer is %T, want *dns.A", resp.Answer[0])
	}
	return a.A.String()
}

func TestSplitRouting(t *testing.T) {
	doh := &fakeDoH{answer: "140.82.112.4", ttl: 60}
	srv := httptest.NewServer(doh.handler())
	defer srv.Close()
	sysAddr := startUDPDNS(t, "192.0.2.1")

	e, err := New(testConfig(config.ModeSplit, srv.URL, sysAddr, "github.com"))
	if err != nil {
		t.Fatal(err)
	}

	// Matched domain -> DoH.
	if got := firstA(t, query(t, e, "api.github.com", dns.TypeA, false)); got != "140.82.112.4" {
		t.Fatalf("matched answer = %s", got)
	}
	// Unmatched domain -> system upstream.
	if got := firstA(t, query(t, e, "example.com", dns.TypeA, false)); got != "192.0.2.1" {
		t.Fatalf("unmatched answer = %s", got)
	}

	st := e.Status()
	if st.DoHQueries != 1 || st.SystemQueries != 1 {
		t.Fatalf("queries = (doh %d, system %d), want (1,1)", st.DoHQueries, st.SystemQueries)
	}
}

func TestFullMode(t *testing.T) {
	doh := &fakeDoH{answer: "140.82.112.4", ttl: 60}
	srv := httptest.NewServer(doh.handler())
	defer srv.Close()
	sysAddr := startUDPDNS(t, "192.0.2.1")

	e, err := New(testConfig(config.ModeFull, srv.URL, sysAddr))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstA(t, query(t, e, "example.com", dns.TypeA, false)); got != "140.82.112.4" {
		t.Fatalf("full-mode answer = %s", got)
	}
	if st := e.Status(); st.SystemQueries != 0 || st.DoHQueries != 1 {
		t.Fatalf("queries = (doh %d, system %d), want (1,0)", st.DoHQueries, st.SystemQueries)
	}
}

func TestCacheHit(t *testing.T) {
	doh := &fakeDoH{answer: "140.82.112.4", ttl: 60}
	srv := httptest.NewServer(doh.handler())
	defer srv.Close()

	e, err := New(testConfig(config.ModeFull, srv.URL, "127.0.0.1:9", "github.com"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := firstA(t, query(t, e, "github.com", dns.TypeA, false)); got != "140.82.112.4" {
			t.Fatalf("answer = %s", got)
		}
	}
	if n := doh.queries.Load(); n != 1 {
		t.Fatalf("upstream queries = %d, want 1 (cache should absorb repeats)", n)
	}
	st := e.Status()
	if st.CacheHits != 2 || st.CacheSize != 1 {
		t.Fatalf("cache stats = (hits %d, size %d), want (2,1)", st.CacheHits, st.CacheSize)
	}
}

func TestNegativeCaching(t *testing.T) {
	doh := &fakeDoH{rcode: dns.RcodeNameError}
	srv := httptest.NewServer(doh.handler())
	defer srv.Close()

	e, err := New(testConfig(config.ModeFull, srv.URL, "127.0.0.1:9", "github.com"))
	if err != nil {
		t.Fatal(err)
	}
	r1 := query(t, e, "missing.github.com", dns.TypeA, false)
	if r1.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %s, want NXDOMAIN", dns.RcodeToString[r1.Rcode])
	}
	r2 := query(t, e, "missing.github.com", dns.TypeA, false)
	if r2.Rcode != dns.RcodeNameError {
		t.Fatalf("cached rcode = %s, want NXDOMAIN", dns.RcodeToString[r2.Rcode])
	}
	if n := doh.queries.Load(); n != 1 {
		t.Fatalf("upstream queries = %d, want 1", n)
	}
}

func TestUpstreamFailure(t *testing.T) {
	doh := &fakeDoH{failHTTP: true}
	srv := httptest.NewServer(doh.handler())
	defer srv.Close()

	e, err := New(testConfig(config.ModeFull, srv.URL, "127.0.0.1:9", "github.com"))
	if err != nil {
		t.Fatal(err)
	}
	resp := query(t, e, "github.com", dns.TypeA, false)
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %s, want SERVFAIL", dns.RcodeToString[resp.Rcode])
	}
	if st := e.Status(); st.Failures != 1 || st.LastFailure == nil {
		t.Fatalf("failures = %d, lastFailure = %v", st.Failures, st.LastFailure)
	}
}

func TestRaceStrategy(t *testing.T) {
	bad := &fakeDoH{failHTTP: true}
	badSrv := httptest.NewServer(bad.handler())
	defer badSrv.Close()
	good := &fakeDoH{answer: "140.82.112.4", ttl: 60}
	goodSrv := httptest.NewServer(good.handler())
	defer goodSrv.Close()

	cfg := testConfig(config.ModeFull, badSrv.URL, "127.0.0.1:9", "github.com")
	cfg.Upstreams.DoH = []string{badSrv.URL, goodSrv.URL}
	cfg.Upstreams.Strategy = config.StrategyRace
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := firstA(t, query(t, e, "github.com", dns.TypeA, false)); got != "140.82.112.4" {
		t.Fatalf("race answer = %s", got)
	}
}

func TestFailoverStrategy(t *testing.T) {
	bad := &fakeDoH{failHTTP: true}
	badSrv := httptest.NewServer(bad.handler())
	defer badSrv.Close()
	good := &fakeDoH{answer: "140.82.112.4", ttl: 60}
	goodSrv := httptest.NewServer(good.handler())
	defer goodSrv.Close()

	cfg := testConfig(config.ModeFull, badSrv.URL, "127.0.0.1:9", "github.com")
	cfg.Upstreams.DoH = []string{badSrv.URL, goodSrv.URL}
	cfg.Upstreams.Strategy = config.StrategyFailover
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := firstA(t, query(t, e, "github.com", dns.TypeA, false)); got != "140.82.112.4" {
		t.Fatalf("failover answer = %s", got)
	}
}

func hasECS(m *dns.Msg) *dns.EDNS0_SUBNET {
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if s, ok := o.(*dns.EDNS0_SUBNET); ok {
				return s
			}
		}
	}
	return nil
}

func clientWithECS(t *testing.T, e *Engine, name string) *dns.Msg {
	t.Helper()
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(name), dns.TypeA)
	req.SetEdns0(4096, false)
	opt := req.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        1,
		SourceNetmask: 24,
		SourceScope:   0,
		Address:       net.ParseIP("10.0.0.0").To4(),
	})
	w := &testWriter{}
	e.ServeDNS(w, req)
	if w.m == nil {
		t.Fatal("no response written")
	}
	if w.m.IsEdns0() == nil {
		t.Error("EDNS client did not get an OPT record back")
	}
	return w.m
}

func TestECSStrip(t *testing.T) {
	// strip=true (default): the DoH upstream must not see ECS.
	stripped := &fakeDoH{answer: "140.82.112.4", ttl: 60, check: func(req *dns.Msg) {
		if s := hasECS(req); s != nil {
			t.Errorf("upstream saw ECS despite strip=true: %v", s)
		}
	}}
	srv := httptest.NewServer(stripped.handler())
	defer srv.Close()
	e, err := New(testConfig(config.ModeFull, srv.URL, "127.0.0.1:9", "github.com"))
	if err != nil {
		t.Fatal(err)
	}
	clientWithECS(t, e, "github.com")

	// strip=false without spoof: ECS passes through.
	passthru := &fakeDoH{answer: "140.82.112.4", ttl: 60, check: func(req *dns.Msg) {
		if s := hasECS(req); s == nil || s.SourceNetmask != 24 {
			t.Errorf("upstream did not see the client ECS: %v", hasECS(req))
		}
	}}
	srv2 := httptest.NewServer(passthru.handler())
	defer srv2.Close()
	cfg := testConfig(config.ModeFull, srv2.URL, "127.0.0.1:9", "github.com")
	cfg.ECS.Strip = false
	e2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clientWithECS(t, e2, "github.com")
}

func TestECSSpoof(t *testing.T) {
	doh := &fakeDoH{answer: "140.82.112.4", ttl: 60, check: func(req *dns.Msg) {
		s := hasECS(req)
		if s == nil {
			t.Error("upstream did not see the spoofed ECS")
			return
		}
		if s.Family != 1 || s.SourceNetmask != 24 || s.Address.String() != "1.2.3.0" {
			t.Errorf("spoofed ECS = %+v", s)
		}
	}}
	srv := httptest.NewServer(doh.handler())
	defer srv.Close()
	cfg := testConfig(config.ModeFull, srv.URL, "127.0.0.1:9", "github.com")
	cfg.ECS.Spoof = "1.2.3.0/24"
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clientWithECS(t, e, "github.com")
}

func TestReload(t *testing.T) {
	doh1 := &fakeDoH{answer: "140.82.112.4", ttl: 60}
	srv1 := httptest.NewServer(doh1.handler())
	defer srv1.Close()
	doh2 := &fakeDoH{answer: "140.82.113.4", ttl: 60}
	srv2 := httptest.NewServer(doh2.handler())
	defer srv2.Close()

	e, err := New(testConfig(config.ModeFull, srv1.URL, "127.0.0.1:9", "github.com"))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstA(t, query(t, e, "github.com", dns.TypeA, false)); got != "140.82.112.4" {
		t.Fatalf("pre-reload answer = %s", got)
	}
	cfg := testConfig(config.ModeFull, srv2.URL, "127.0.0.1:9", "github.com")
	if err := e.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if got := firstA(t, query(t, e, "github.com", dns.TypeA, false)); got != "140.82.113.4" {
		t.Fatalf("post-reload answer = %s", got)
	}
}

func TestSystemFallbackChain(t *testing.T) {
	// System upstream is dead (closed port); the configured public fallback
	// must answer.
	fbAddr := startUDPDNS(t, "192.0.2.53")
	cfg := testConfig(config.ModeSplit, "http://127.0.0.1:9/dns-query", "127.0.0.1:1", "github.com")
	cfg.Upstreams.Fallback = []string{fbAddr}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := firstA(t, query(t, e, "example.com", dns.TypeA, false)); got != "192.0.2.53" {
		t.Fatalf("fallback answer = %s", got)
	}
	st := e.Status()
	if st.FallbackQueries != 1 {
		t.Fatalf("fallback queries = %d, want 1", st.FallbackQueries)
	}
	if len(st.SystemFallbackUpstreams) != 1 || st.SystemFallbackUpstreams[0] != fbAddr {
		t.Fatalf("fallback upstreams = %v", st.SystemFallbackUpstreams)
	}
}

func TestOverrideRouting(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "over.hosts")
	if err := os.WriteFile(f, []byte("10.9.8.7 pinned.github.com\n2001:db8::1 pinned.github.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doh := &fakeDoH{answer: "140.82.112.4", ttl: 60}
	srv := httptest.NewServer(doh.handler())
	defer srv.Close()

	cfg := testConfig(config.ModeSplit, srv.URL, "127.0.0.1:9", "github.com")
	cfg.Override.Files = []string{f}
	cfg.Override.TTL = 30 * time.Second
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown()

	// Override wins over the polluted-domain DoH routing.
	resp := query(t, e, "pinned.github.com", dns.TypeA, false)
	if got := firstA(t, resp); got != "10.9.8.7" {
		t.Fatalf("override A = %s", got)
	}
	if n := doh.queries.Load(); n != 0 {
		t.Fatalf("DoH queries = %d, want 0 (override must not hit upstreams)", n)
	}
	// AAAA record from the same table entry.
	resp = query(t, e, "pinned.github.com", dns.TypeAAAA, false)
	if len(resp.Answer) != 1 {
		t.Fatalf("AAAA answers = %d", len(resp.Answer))
	}
	if aaaa, ok := resp.Answer[0].(*dns.AAAA); !ok || aaaa.AAAA.String() != "2001:db8::1" {
		t.Fatalf("AAAA answer = %v", resp.Answer[0])
	}
	// Second A query is served from cache, not counted as override query.
	_ = query(t, e, "pinned.github.com", dns.TypeA, false)
	st := e.Status()
	if st.OverrideQueries != 2 {
		t.Fatalf("override queries = %d, want 2", st.OverrideQueries)
	}
	if st.OverrideEntries != 1 {
		t.Fatalf("override entries = %d, want 1", st.OverrideEntries)
	}
}

func TestNormalizeAddr(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":      "127.0.0.1:53",
		"127.0.0.1:5353": "127.0.0.1:5353",
		"::1":            "[::1]:53",
		"[::1]:53":       "[::1]:53",
		"0.0.0.0:53":     "0.0.0.0:53",
	}
	for in, want := range cases {
		got, err := NormalizeAddr(in)
		if err != nil || got != want {
			t.Errorf("NormalizeAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeAddr(""); err == nil {
		t.Error("expected error for empty address")
	}
}
