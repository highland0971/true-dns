package cache

import (
	"testing"
	"time"

	"github.com/miekg/dns"
)

func newMsg(name string, ttl uint32) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: dns.Fqdn(name), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
		A:   []byte{1, 2, 3, 4},
	}}
	return m
}

func TestGetPut(t *testing.T) {
	c := New(16, time.Hour)
	q := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	key := Key(q)

	if _, ok := c.Get(key); ok {
		t.Fatal("empty cache returned a hit")
	}
	c.Put(key, newMsg("example.com.", 300), 300*time.Second)
	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got.Answer) != 1 || got.Answer[0].Header().Ttl != 300 {
		t.Fatalf("unexpected cached answer: %v", got.Answer)
	}
	size, hits, misses := c.Stats()
	if size != 1 || hits != 1 || misses != 1 {
		t.Fatalf("stats = (%d,%d,%d), want (1,1,1)", size, hits, misses)
	}
}

func TestExpiry(t *testing.T) {
	c := New(16, time.Hour)
	key := Key(dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	c.Put(key, newMsg("example.com.", 300), 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if _, ok := c.Get(key); ok {
		t.Fatal("expired entry was returned")
	}
}

func TestMaxTTLCap(t *testing.T) {
	c := New(16, 60*time.Second)
	key := Key(dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	c.Put(key, newMsg("example.com.", 300), 300*time.Second)
	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Answer[0].Header().Ttl != 60 {
		t.Fatalf("TTL = %d, want capped 60", got.Answer[0].Header().Ttl)
	}
}

func TestTTLDecrement(t *testing.T) {
	c := New(16, time.Hour)
	key := Key(dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	c.Put(key, newMsg("example.com.", 300), 2*time.Second)
	time.Sleep(1100 * time.Millisecond)
	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if ttl := got.Answer[0].Header().Ttl; ttl > 1 {
		t.Fatalf("TTL = %d, want decremented to 1", ttl)
	}
}

func TestEviction(t *testing.T) {
	c := New(2, time.Hour)
	k1 := Key(dns.Question{Name: "a.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	k2 := Key(dns.Question{Name: "b.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	k3 := Key(dns.Question{Name: "c.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	c.Put(k1, newMsg("a.example.", 60), time.Minute)
	c.Put(k2, newMsg("b.example.", 60), time.Minute)
	c.Put(k3, newMsg("c.example.", 60), time.Minute) // evicts a
	if _, ok := c.Get(k1); ok {
		t.Fatal("expected LRU eviction of a")
	}
	if _, ok := c.Get(k2); !ok {
		t.Fatal("b should still be cached")
	}
	if _, ok := c.Get(k3); !ok {
		t.Fatal("c should still be cached")
	}
}

func TestFlush(t *testing.T) {
	c := New(16, time.Hour)
	key := Key(dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	c.Put(key, newMsg("example.com.", 300), time.Minute)
	c.Flush()
	if _, ok := c.Get(key); ok {
		t.Fatal("flush left entries behind")
	}
}

func TestTTLFromMsg(t *testing.T) {
	// Positive answer: min TTL across records wins.
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	m.Answer = []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: []byte{1, 1, 1, 1}},
		&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{2, 2, 2, 2}},
	}
	if got := TTLFromMsg(m, time.Hour); got != 60*time.Second {
		t.Fatalf("TTLFromMsg = %v, want 60s", got)
	}
	// Negative answer: SOA in authority.
	nx := new(dns.Msg)
	nx.SetRcode(m, dns.RcodeNameError)
	nx.Ns = []dns.RR{&dns.SOA{
		Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 100},
		Ns:  "ns.example.com.", Mbox: "host.example.com.",
		Serial: 1, Refresh: 2, Retry: 3, Expire: 4, Minttl: 30,
	}}
	if got := TTLFromMsg(nx, time.Hour); got != 30*time.Second {
		t.Fatalf("TTLFromMsg(negative) = %v, want 30s", got)
	}
	// SERVFAIL: no TTL-bearing records.
	fail := new(dns.Msg)
	fail.SetRcode(m, dns.RcodeServerFailure)
	if got := TTLFromMsg(fail, time.Hour); got != 0 {
		t.Fatalf("TTLFromMsg(servfail) = %v, want 0", got)
	}
	// Cap.
	if got := TTLFromMsg(newMsg("example.com.", 3600), 600*time.Second); got != 600*time.Second {
		t.Fatalf("TTLFromMsg(cap) = %v, want 600s", got)
	}
}
