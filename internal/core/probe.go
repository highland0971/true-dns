package core

import (
	"net"

	"github.com/miekg/dns"

	"truedns/internal/config"
	"truedns/internal/probe"
)

// newProber builds the reachability prober for a configuration (nil when
// probe.enabled is false).
func newProber(cfg *config.Config) *probe.Prober {
	if !cfg.Probe.Enabled {
		return nil
	}
	return probe.New(cfg.Probe.Port, cfg.Probe.Timeout, cfg.Probe.CacheTTL)
}

// filterAnswerIPs reshapes the A/AAAA records of a DoH answer according to
// the probe configuration (drop unreachable IPs or prefer reachable ones).
// Non-address records are preserved; per-family sets smaller than 2 are left
// untouched.
func filterAnswerIPs(answer []dns.RR, pr *probe.Prober, cfg config.ProbeConfig) []dns.RR {
	mode := probe.Mode(cfg.Mode)
	byIP := make(map[string]dns.RR)
	var v4, v6 []net.IP
	var others []dns.RR
	for _, rr := range answer {
		switch r := rr.(type) {
		case *dns.A:
			byIP[r.A.String()] = rr
			v4 = append(v4, r.A)
		case *dns.AAAA:
			byIP[r.AAAA.String()] = rr
			v6 = append(v6, r.AAAA)
		default:
			others = append(others, rr)
		}
	}
	if len(v4) <= 1 && len(v6) <= 1 {
		return answer
	}
	v4 = pr.Filter(v4, mode, cfg.MaxIPs)
	v6 = pr.Filter(v6, mode, cfg.MaxIPs)
	out := make([]dns.RR, 0, len(answer))
	out = append(out, others...)
	for _, ip := range v4 {
		out = append(out, byIP[ip.String()])
	}
	for _, ip := range v6 {
		out = append(out, byIP[ip.String()])
	}
	return out
}
