package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// Plain is a classic plaintext DNS upstream. Queries go out over UDP with a
// TCP retry when the response is truncated; addresses are tried round-robin.
type Plain struct {
	addrs []string
	udp   *dns.Client
	tcp   *dns.Client
	next  atomic.Uint64
}

// NewPlain creates a Plain upstream for the given addresses ("1.2.3.4" or
// "1.2.3.4:5353"; a missing port defaults to 53).
func NewPlain(addrs []string, timeout time.Duration) *Plain {
	norm := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if na, err := WithPort(a); err == nil {
			norm = append(norm, na)
		}
	}
	return &Plain{
		addrs: norm,
		udp:   &dns.Client{Net: "udp", Timeout: timeout},
		tcp:   &dns.Client{Net: "tcp", Timeout: timeout},
	}
}

// Addrs returns the configured upstream addresses.
func (p *Plain) Addrs() []string { return p.addrs }

// Exchange queries the addresses round-robin until one answers. On a
// truncated UDP reply it retries the same address over TCP.
func (p *Plain) Exchange(ctx context.Context, req *dns.Msg) (*dns.Msg, error) {
	if len(p.addrs) == 0 {
		return nil, errors.New("no plain upstream addresses configured")
	}
	start := int((p.next.Add(1) - 1) % uint64(len(p.addrs)))
	var lastErr error
	for i := 0; i < len(p.addrs); i++ {
		addr := p.addrs[(start+i)%len(p.addrs)]
		resp, _, err := p.udp.ExchangeContext(ctx, req, addr)
		if err != nil {
			lastErr = fmt.Errorf("udp %s: %w", addr, err)
			continue
		}
		if resp.Truncated {
			if t, _, terr := p.tcp.ExchangeContext(ctx, req, addr); terr == nil {
				return t, nil
			} else {
				lastErr = fmt.Errorf("tcp %s: %w", addr, terr)
				continue
			}
		}
		return resp, nil
	}
	return nil, lastErr
}

// WithPort normalizes a DNS server address, appending port 53 when missing.
func WithPort(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("empty address")
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr, nil
	}
	if strings.HasPrefix(addr, "[") || strings.Contains(addr, ":") {
		// Bare IPv6 literal.
		if ip := net.ParseIP(strings.Trim(addr, "[]")); ip != nil {
			return net.JoinHostPort(ip.String(), "53"), nil
		}
		return "", fmt.Errorf("invalid address %q", addr)
	}
	if net.ParseIP(addr) != nil {
		return net.JoinHostPort(addr, "53"), nil
	}
	return "", fmt.Errorf("invalid address %q", addr)
}
