//go:build linux

package platform

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func init() { current = linuxPlatform{} }

type linuxPlatform struct{}

func (linuxPlatform) Name() string { return "linux" }

const (
	resolvConf     = "/etc/resolv.conf"
	resolvedDir    = "/etc/systemd/resolved.conf.d"
	resolvedDropin = resolvedDir + "/truedns.conf"
)

// usesSystemdResolved reports whether DNS resolution is managed by
// systemd-resolved (resolv.conf is a symlink into its runtime, or its stub
// file exists).
func usesSystemdResolved() bool {
	if target, err := os.Readlink(resolvConf); err == nil && strings.Contains(target, "systemd") {
		return true
	}
	_, err := os.Stat("/run/systemd/resolve/stub-resolv.conf")
	return err == nil
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("root privileges required (run with sudo)")
	}
	return nil
}

// linuxPayload records what was changed so restore can undo it.
type linuxPayload struct {
	// Manager is "systemd-resolved" or "resolv.conf".
	Manager string `json:"manager"`
	// PrevDropin is the previous content of the resolved drop-in file.
	PrevDropin string `json:"prev_dropin,omitempty"`
	// PrevResolvConf is the previous content of /etc/resolv.conf.
	PrevResolvConf string `json:"prev_resolv_conf,omitempty"`
}

// SetSystemDNS points the system resolver at 127.0.0.1:
//   - systemd-resolved: a drop-in sets DNS=127.0.0.1 (the stub listener at
//     127.0.0.53 keeps serving clients and forwards to our proxy), then the
//     service is restarted.
//   - classic resolv.conf: the file is replaced (original kept in state,
//     non-nameserver lines preserved).
func (linuxPlatform) SetSystemDNS() (*TakeoverState, error) {
	if err := requireRoot(); err != nil {
		return nil, err
	}
	p := linuxPayload{}
	if usesSystemdResolved() {
		p.Manager = "systemd-resolved"
		prev, _ := os.ReadFile(resolvedDropin)
		p.PrevDropin = string(prev)
		if err := os.MkdirAll(resolvedDir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", resolvedDir, err)
		}
		content := "# Managed by truedns. Delete this file to restore the previous configuration.\n[Resolve]\nDNS=127.0.0.1\n"
		if err := os.WriteFile(resolvedDropin, []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", resolvedDropin, err)
		}
		if out, err := exec.Command("systemctl", "restart", "systemd-resolved").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("restart systemd-resolved: %v: %s", err, out)
		}
		return newState(p)
	}

	p.Manager = "resolv.conf"
	prev, err := os.ReadFile(resolvConf)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", resolvConf, err)
	}
	p.PrevResolvConf = string(prev)
	var lines []string
	lines = append(lines, "# Managed by truedns. Run \"truedns restore\" to revert.")
	lines = append(lines, "nameserver 127.0.0.1")
	for _, l := range strings.Split(p.PrevResolvConf, "\n") {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "nameserver") {
			continue
		}
		lines = append(lines, l)
	}
	if err := os.WriteFile(resolvConf, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", resolvConf, err)
	}
	return newState(p)
}

// RestoreSystemDNS undoes the takeover recorded in state.
func (linuxPlatform) RestoreSystemDNS(state *TakeoverState) error {
	if err := requireRoot(); err != nil {
		return err
	}
	var p linuxPayload
	if err := json.Unmarshal(state.Payload, &p); err != nil {
		return fmt.Errorf("bad state payload: %w", err)
	}
	switch p.Manager {
	case "systemd-resolved":
		if p.PrevDropin == "" {
			_ = os.Remove(resolvedDropin)
		} else if err := os.WriteFile(resolvedDropin, []byte(p.PrevDropin), 0o644); err != nil {
			return fmt.Errorf("restore %s: %w", resolvedDropin, err)
		}
		if out, err := exec.Command("systemctl", "restart", "systemd-resolved").CombinedOutput(); err != nil {
			return fmt.Errorf("restart systemd-resolved: %v: %s", err, out)
		}
	case "resolv.conf":
		if p.PrevResolvConf == "" {
			return fmt.Errorf("state contains no previous resolv.conf content")
		}
		if err := os.WriteFile(resolvConf, []byte(p.PrevResolvConf), 0o644); err != nil {
			return fmt.Errorf("restore %s: %w", resolvConf, err)
		}
	default:
		return fmt.Errorf("unknown takeover manager %q in state", p.Manager)
	}
	return nil
}

// FlushDNSCache clears resolver caches where possible.
func (linuxPlatform) FlushDNSCache() error {
	if usesSystemdResolved() {
		// Either binary name may exist depending on the distribution.
		_ = exec.Command("resolvectl", "flush-caches").Run()
		_ = exec.Command("systemd-resolve", "--flush-caches").Run()
	}
	// nscd/dnsmasq restarts are distribution-specific; treat as best effort.
	_ = exec.Command("systemctl", "restart", "nscd").Run()
	return nil
}

// DescribeState summarizes a takeover state for humans.
func (linuxPlatform) DescribeState(state *TakeoverState) string {
	var p linuxPayload
	if json.Unmarshal(state.Payload, &p) != nil {
		return fmt.Sprintf("linux takeover taken at %s", state.TakenAt.Format("2006-01-02 15:04:05"))
	}
	return fmt.Sprintf("linux (%s): system DNS points to 127.0.0.1 (taken at %s)", p.Manager, state.TakenAt.Format("2006-01-02 15:04:05"))
}

// DiscoverSystemDNS prefers the pre-takeover configuration from saved state,
// then the live /etc/resolv.conf. Loopback entries (the proxy itself) are
// filtered, with public fallbacks as a last resort.
func (linuxPlatform) DiscoverSystemDNS() []string {
	content := ""
	if data, err := os.ReadFile(resolvConf); err == nil {
		content = string(data)
	}
	var raw []string
	if st, err := LoadState(); err == nil {
		var p linuxPayload
		if json.Unmarshal(st.Payload, &p) == nil {
			switch p.Manager {
			case "resolv.conf":
				if p.PrevResolvConf != "" {
					content = p.PrevResolvConf
				}
			case "systemd-resolved":
				for _, line := range strings.Split(p.PrevDropin, "\n") {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "DNS=") {
						raw = append(raw, strings.Fields(strings.TrimPrefix(line, "DNS="))...)
					}
				}
			}
		}
	}
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			raw = append(raw, f[1])
		}
	}
	got := normalizeAddrs(raw)
	if len(got) == 0 {
		got = normalizeAddrs(DefaultFallbackServers)
	}
	return got
}
