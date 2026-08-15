// Package resolver implements the upstream DNS clients used by the engine:
// an encrypted DNS-over-HTTPS client and a classic plaintext (UDP, TCP
// fallback) client.
package resolver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/miekg/dns"

	"truedns/internal/version"
)

const dohMime = "application/dns-message"

// ErrMismatched indicates an upstream response that does not correspond to the
// query (wrong ID or question) — such a response must never be forwarded.
var ErrMismatched = errors.New("upstream response does not match the query")

// DoH is a DNS-over-HTTPS upstream.
type DoH struct {
	Name    string
	URL     *url.URL
	http    *http.Client
	timeout time.Duration
}

// NewDoH creates a DoH upstream. rawURL must be an http(s) URL. proxyURL is an
// optional HTTP(S) proxy used for the upstream connection.
func NewDoH(name, rawURL string, timeout time.Duration, proxyURL string) (*DoH, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("bad DoH URL %q: %w", rawURL, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("bad DoH URL %q: scheme must be http(s)", rawURL)
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	if proxyURL != "" {
		pu, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("bad proxy_url %q: %w", proxyURL, err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	return &DoH{
		Name:    name,
		URL:     u,
		http:    &http.Client{Transport: tr},
		timeout: timeout,
	}, nil
}

// Exchange performs a DoH query. It first tries RFC 8484 POST; on failure it
// retries once with the GET form, which some deployments require. Responses
// are validated against the query before being returned.
func (d *DoH) Exchange(ctx context.Context, req *dns.Msg) (*dns.Msg, error) {
	wire, err := req.Pack()
	if err != nil {
		return nil, fmt.Errorf("pack query: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	resp, err := d.post(ctx, wire)
	if err == nil {
		if err := ValidateAgainst(req, resp); err != nil {
			return nil, err
		}
		return resp, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}
	// Fall back to the GET form once; some endpoints reject POST.
	resp, err = d.get(ctx, wire)
	if err != nil {
		return nil, err
	}
	if err := ValidateAgainst(req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (d *DoH) post(ctx context.Context, wire []byte) (*dns.Msg, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL.String(), bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", dohMime)
	hreq.Header.Set("Accept", dohMime)
	hreq.Header.Set("User-Agent", "truedns/"+version.Version)
	hresp, err := d.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("doh %s: %w", d.Name, err)
	}
	defer hresp.Body.Close()
	return d.readResponse(hresp)
}

func (d *DoH) get(ctx context.Context, wire []byte) (*dns.Msg, error) {
	u := *d.URL
	q := u.Query()
	q.Set("dns", base64.RawURLEncoding.EncodeToString(wire))
	u.RawQuery = q.Encode()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Accept", dohMime)
	hreq.Header.Set("User-Agent", "truedns/"+version.Version)
	hresp, err := d.http.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("doh %s (GET): %w", d.Name, err)
	}
	defer hresp.Body.Close()
	return d.readResponse(hresp)
}

func (d *DoH) readResponse(hresp *http.Response) (*dns.Msg, error) {
	body, err := io.ReadAll(io.LimitReader(hresp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("doh %s: read body: %w", d.Name, err)
	}
	if hresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh %s: http %d", d.Name, hresp.StatusCode)
	}
	m := new(dns.Msg)
	if err := m.Unpack(body); err != nil {
		return nil, fmt.Errorf("doh %s: unpack response: %w", d.Name, err)
	}
	return m, nil
}

// ValidateAgainst checks that resp plausibly answers req. A mismatched
// response (wrong ID or question) must never be forwarded to the client.
func ValidateAgainst(req, resp *dns.Msg) error {
	if resp == nil {
		return errors.New("nil upstream response")
	}
	if resp.Id != req.Id {
		return ErrMismatched
	}
	if len(req.Question) == 1 && len(resp.Question) >= 1 {
		rq, sq := req.Question[0], resp.Question[0]
		if !strings.EqualFold(rq.Name, sq.Name) || rq.Qtype != sq.Qtype || rq.Qclass != sq.Qclass {
			return ErrMismatched
		}
	}
	return nil
}
