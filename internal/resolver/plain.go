package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Plain is a classic plaintext DNS upstream. All configured addresses are
// queried concurrently (first valid answer wins) with a TCP retry per address
// when its UDP response is truncated.
type Plain struct {
	addrs []string
	udp   *dns.Client
	tcp   *dns.Client
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

// Exchange queries all addresses concurrently and returns the first valid
// answer (dead upstreams — e.g. VM host-only adapters — cannot delay the
// query). On a truncated UDP reply it retries the same address over TCP.
func (p *Plain) Exchange(ctx context.Context, req *dns.Msg) (*dns.Msg, error) {
	if len(p.addrs) == 0 {
		return nil, errors.New("no plain upstream addresses configured")
	}
	if len(p.addrs) == 1 {
		return p.exchangeAddr(ctx, req, p.addrs[0])
	}
	type result struct {
		resp *dns.Msg
		err  error
	}
	results := make(chan result, len(p.addrs))
	for _, addr := range p.addrs {
		go func(addr string) {
			resp, err := p.exchangeAddr(ctx, req, addr)
			select {
			case results <- result{resp, err}:
			case <-ctx.Done():
			}
		}(addr)
	}
	var firstErr error
	for range p.addrs {
		select {
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			return nil, firstErr
		case r := <-results:
			if r.err != nil {
				if firstErr == nil {
					firstErr = r.err
				}
				continue
			}
			return r.resp, nil
		}
	}
	return nil, firstErr
}

func (p *Plain) exchangeAddr(ctx context.Context, req *dns.Msg, addr string) (*dns.Msg, error) {
	resp, _, err := p.udp.ExchangeContext(ctx, req, addr)
	if err != nil {
		return nil, fmt.Errorf("udp %s: %w", addr, err)
	}
	if resp.Truncated {
		t, _, terr := p.tcp.ExchangeContext(ctx, req, addr)
		if terr != nil {
			return nil, fmt.Errorf("tcp %s: %w", addr, terr)
		}
		return t, nil
	}
	return resp, nil
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
