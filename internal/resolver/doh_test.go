package resolver

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// cannedResponse builds a wire response for any A query.
func cannedResponse(req *dns.Msg, ip string, ttl uint32) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
		A:   net.ParseIP(ip).To4(),
	}}
	return m
}

func TestDoHPost(t *testing.T) {
	var gotPost bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != dohMime {
			t.Errorf("Content-Type = %q", ct)
		}
		gotPost = true
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		req := new(dns.Msg)
		if err := req.Unpack(buf[:n]); err != nil {
			t.Fatalf("unpack request: %v", err)
		}
		wire, _ := cannedResponse(req, "9.9.9.9", 300).Pack()
		w.Header().Set("Content-Type", dohMime)
		w.Write(wire)
	}))
	defer srv.Close()

	d, err := NewDoH("test", srv.URL, 2*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, err := d.Exchange(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !gotPost {
		t.Error("server did not receive a POST")
	}
	if err := ValidateAgainst(q, resp); err != nil {
		t.Fatalf("response validation failed: %v", err)
	}
	if len(resp.Answer) != 1 || resp.Answer[0].(*dns.A).A.String() != "9.9.9.9" {
		t.Fatalf("unexpected answer: %v", resp.Answer)
	}
}

func TestDoHGetFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			http.Error(w, "POST disabled", http.StatusMethodNotAllowed)
		case http.MethodGet:
			enc := r.URL.Query().Get("dns")
			if enc == "" {
				http.Error(w, "missing dns param", http.StatusBadRequest)
				return
			}
			wire, err := base64URLDecode(enc)
			if err != nil {
				http.Error(w, "bad dns param", http.StatusBadRequest)
				return
			}
			req := new(dns.Msg)
			if err := req.Unpack(wire); err != nil {
				http.Error(w, "unpack", http.StatusBadRequest)
				return
			}
			out, _ := cannedResponse(req, "8.8.4.4", 60).Pack()
			w.Header().Set("Content-Type", dohMime)
			w.Write(out)
		}
	}))
	defer srv.Close()

	d, err := NewDoH("test", srv.URL, 2*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, err := d.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("GET fallback failed: %v", err)
	}
	if a := resp.Answer[0].(*dns.A).A.String(); a != "8.8.4.4" {
		t.Fatalf("answer = %s", a)
	}
}

func TestDoHValidationMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := new(dns.Msg)
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		if err := req.Unpack(buf[:n]); err != nil {
			t.Fatal(err)
		}
		req.Id++ // tamper: response for a different transaction
		wire, _ := cannedResponse(req, "1.1.1.1", 60).Pack()
		w.Header().Set("Content-Type", dohMime)
		w.Write(wire)
	}))
	defer srv.Close()

	d, _ := NewDoH("test", srv.URL, 2*time.Second, "")
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	if _, err := d.Exchange(context.Background(), q); err == nil {
		t.Fatal("expected error for mismatched response")
	}
}

// TestDoHLive pings real public DoH endpoints and is skipped automatically
// when the environment has no route to them (e.g. sandboxes).
func TestDoHLive(t *testing.T) {
	endpoints := []string{
		"https://dns.alidns.com/dns-query",
		"https://dns.google/dns-query",
		"https://cloudflare-dns.com/dns-query",
	}
	ok := false
	for _, ep := range endpoints {
		d, err := NewDoH("live", ep, 4*time.Second, "")
		if err != nil {
			t.Logf("%s: %v", ep, err)
			continue
		}
		q := new(dns.Msg)
		q.SetQuestion("example.com.", dns.TypeA)
		resp, err := d.Exchange(context.Background(), q)
		if err != nil {
			t.Logf("%s unreachable: %v", ep, err)
			continue
		}
		if len(resp.Answer) == 0 {
			t.Logf("%s returned no answer", ep)
			continue
		}
		t.Logf("%s OK: %s -> %v", ep, q.Question[0].Name, resp.Answer)
		ok = true
	}
	if !ok {
		t.Skip("no live DoH endpoint reachable from this environment")
	}
}
