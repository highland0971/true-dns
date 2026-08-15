package resolver

import (
	"context"
	"encoding/base64"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// startPlainServer runs a UDP+TCP DNS server pair on the same ephemeral port
// and returns the shared address. udpHandler and tcpHandler receive the query
// and return the response to send over the respective transport.
func startPlainServer(t *testing.T, udpHandler, tcpHandler func(req *dns.Msg) *dns.Msg) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	udpH := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		if err := w.WriteMsg(udpHandler(r)); err != nil {
			t.Logf("udp write: %v", err)
		}
	})
	tcpH := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		if err := w.WriteMsg(tcpHandler(r)); err != nil {
			t.Logf("tcp write: %v", err)
		}
	})
	udpSrv := &dns.Server{PacketConn: pc, Handler: udpH}
	tcpSrv := &dns.Server{Listener: ln, Handler: tcpH}
	go func() { _ = udpSrv.ActivateAndServe() }()
	go func() { _ = tcpSrv.ActivateAndServe() }()
	t.Cleanup(func() { udpSrv.Shutdown(); tcpSrv.Shutdown() })
	return addr
}

func TestPlainExchange(t *testing.T) {
	full := func(req *dns.Msg) *dns.Msg { return cannedResponse(req, "10.0.0.1", 60) }
	addr := startPlainServer(t, full, full)
	p := NewPlain([]string{addr}, 2*time.Second)
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, err := p.Exchange(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.Answer[0].(*dns.A).A.String(); a != "10.0.0.1" {
		t.Fatalf("answer = %s", a)
	}
}

func TestPlainTCPFallback(t *testing.T) {
	truncated := func(req *dns.Msg) *dns.Msg {
		m := cannedResponse(req, "10.0.0.2", 60)
		m.Truncated = true // force TCP retry
		return m
	}
	full := func(req *dns.Msg) *dns.Msg { return cannedResponse(req, "10.0.0.2", 60) }
	addr := startPlainServer(t, truncated, full)
	p := NewPlain([]string{addr}, 2*time.Second)
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, err := p.Exchange(context.Background(), q)
	if err != nil {
		t.Fatalf("TCP fallback failed: %v", err)
	}
	if resp.Truncated {
		t.Fatal("TCP response still truncated")
	}
	if a := resp.Answer[0].(*dns.A).A.String(); a != "10.0.0.2" {
		t.Fatalf("answer = %s", a)
	}
}

func TestWithPort(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":         "1.2.3.4:53",
		"1.2.3.4:5353":    "1.2.3.4:5353",
		"::1":             "[::1]:53",
		"[::1]:53":        "[::1]:53",
		"2001:db8::1":     "[2001:db8::1]:53",
		"dns.example.com": "",
	}
	for in, want := range cases {
		got, err := WithPort(in)
		if want == "" {
			if err == nil {
				t.Errorf("WithPort(%q) = %q, want error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("WithPort(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
