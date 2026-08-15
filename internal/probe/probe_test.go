package probe

import (
	"net"
	"sync"
	"testing"
	"time"
)

// startTCPListener binds an ephemeral loopback TCP port and returns its port.
// Only 127.0.0.1 answers on it; 127.0.0.2 gets connection-refused, giving two
// distinct reachability outcomes under one Prober.
func startTCPListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestCheckReachableAndCache(t *testing.T) {
	port := startTCPListener(t)
	p := New(port, 500*time.Millisecond, time.Minute)
	res := p.Check(net.ParseIP("127.0.0.1"))
	if !res.Reachable {
		t.Fatal("expected reachable")
	}
	if res.Latency < 0 {
		t.Fatal("negative latency")
	}
	// Cached second check must not re-dial.
	_ = p.Check(net.ParseIP("127.0.0.1"))
	if got := p.Stats(); got != 1 {
		t.Fatalf("probe count = %d, want 1 (cached)", got)
	}
}

func TestCheckUnreachable(t *testing.T) {
	port := startTCPListener(t)
	p := New(port, 300*time.Millisecond, time.Minute)
	if res := p.Check(net.ParseIP("127.0.0.2")); res.Reachable {
		t.Fatal("expected unreachable (nothing bound on 127.0.0.2)")
	}
}

func TestFilterDrop(t *testing.T) {
	port := startTCPListener(t)
	p := New(port, 500*time.Millisecond, time.Minute)
	reachable := net.ParseIP("127.0.0.1")
	unreachable := net.ParseIP("127.0.0.2")

	// Mixed set: unreachable dropped, reachable kept.
	got := p.Filter([]net.IP{unreachable, reachable}, ModeDrop, 8)
	if len(got) != 1 || !got[0].Equal(reachable) {
		t.Fatalf("drop result = %v", got)
	}
	// All unreachable: original set kept (transient-failure fallback).
	got = p.Filter([]net.IP{unreachable}, ModeDrop, 8)
	if len(got) != 1 || !got[0].Equal(unreachable) {
		t.Fatalf("all-unreachable result = %v", got)
	}
	// Single IP: returned untouched without probing.
	got = p.Filter([]net.IP{unreachable}, ModeDrop, 8)
	if len(got) != 1 {
		t.Fatalf("single result = %v", got)
	}
}

func TestFilterPrefer(t *testing.T) {
	port := startTCPListener(t)
	p := New(port, 500*time.Millisecond, time.Minute)
	reachable := net.ParseIP("127.0.0.1")
	unreachable := net.ParseIP("127.0.0.2")

	got := p.Filter([]net.IP{unreachable, reachable}, ModePrefer, 8)
	if len(got) != 2 {
		t.Fatalf("prefer result = %v", got)
	}
	if !got[0].Equal(reachable) || !got[1].Equal(unreachable) {
		t.Fatalf("prefer order = %v", got)
	}
}

func TestConcurrentDedup(t *testing.T) {
	port := startTCPListener(t)
	p := New(port, 500*time.Millisecond, time.Minute)
	ip := net.ParseIP("127.0.0.2") // unreachable
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Check(ip)
		}()
	}
	wg.Wait()
	if got := p.Stats(); got != 1 {
		t.Fatalf("dials = %d, want 1 (concurrent misses must share one dial)", got)
	}
}

func TestFilterMax(t *testing.T) {
	port := startTCPListener(t)
	p := New(port, 500*time.Millisecond, time.Minute)
	ips := []net.IP{
		net.ParseIP("127.0.0.2"), // unreachable
		net.ParseIP("127.0.0.1"), // reachable
		net.ParseIP("127.0.0.3"), // beyond max: untouched
	}
	got := p.Filter(ips, ModeDrop, 2)
	// First two probed: 127.0.0.1 kept; 127.0.0.3 untouched and appended.
	if len(got) != 2 || !got[0].Equal(net.ParseIP("127.0.0.1")) || !got[1].Equal(net.ParseIP("127.0.0.3")) {
		t.Fatalf("max-limited result = %v", got)
	}
}
