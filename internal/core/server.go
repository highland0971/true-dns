package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Serve starts UDP+TCP DNS listeners on each configured address and runs
// until ctx is done or a listener fails fatally.
func (e *Engine) Serve(ctx context.Context) error {
	var servers []*dns.Server
	for _, raw := range e.cfg.Listen {
		addr, err := NormalizeAddr(raw)
		if err != nil {
			return err
		}
		for _, netw := range []string{"udp", "tcp"} {
			servers = append(servers, &dns.Server{Addr: addr, Net: netw, Handler: e})
		}
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *dns.Server) {
			slog.Info("listening", "net", s.Net, "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil {
				errCh <- fmt.Errorf("%s/%s: %w", s.Net, s.Addr, portHint(err))
			}
		}(s)
	}
	select {
	case <-ctx.Done():
		slog.Info("shutting down DNS listeners")
		shutdownAll(servers)
		return nil
	case err := <-errCh:
		shutdownAll(servers)
		return err
	}
}

func shutdownAll(servers []*dns.Server) {
	for _, s := range servers {
		shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.ShutdownContext(shCtx)
		cancel()
	}
}

// NormalizeAddr appends :53 to a bare listen address when no port is given.
func NormalizeAddr(a string) (string, error) {
	a = strings.TrimSpace(a)
	if a == "" {
		return "", errors.New("empty listen address")
	}
	if _, _, err := net.SplitHostPort(a); err == nil {
		return a, nil
	}
	if strings.HasPrefix(a, "[") || strings.Contains(a, ":") {
		if ip := net.ParseIP(strings.Trim(a, "[]")); ip != nil {
			return net.JoinHostPort(ip.String(), "53"), nil
		}
		return "", fmt.Errorf("invalid listen address %q", a)
	}
	if net.ParseIP(a) != nil {
		return net.JoinHostPort(a, "53"), nil
	}
	return "", fmt.Errorf("invalid listen address %q", a)
}

// portHint augments common bind errors with actionable guidance.
func portHint(err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage") ||
		strings.Contains(msg, "access is denied") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "address in use") {
		return fmt.Errorf("%w (hint: port 53 is busy or needs privileges — on Windows stop the Internet Connection Sharing service \"SharedAccess\" if it holds port 53; on Linux stop the systemd-resolved stub listener / dnsmasq, or pick a different listen port)", err)
	}
	return err
}
