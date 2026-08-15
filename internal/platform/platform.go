// Package platform abstracts OS-specific system-DNS takeover and system DNS
// discovery. Each OS has its own implementation selected by build tags, so
// the core engine stays fully portable. Adding a new OS means implementing
// the Platform interface in a new build-tagged file.
package platform

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"truedns/internal/paths"
	"truedns/internal/version"
)

// TakeoverState is persisted to disk so that restore works across reboots
// and even after the tool exits unexpectedly (e.g. power loss). The payload
// is platform-specific JSON.
type TakeoverState struct {
	Tool     string          `json:"tool"`
	Version  string          `json:"version"`
	Platform string          `json:"platform"`
	TakenAt  time.Time       `json:"taken_at"`
	Payload  json.RawMessage `json:"payload"`
}

// Platform is the OS-specific behavior the engine and CLI rely on.
type Platform interface {
	Name() string
	// SetSystemDNS points the system resolver at the local proxy (127.0.0.1)
	// and returns the state required to undo the change.
	SetSystemDNS() (*TakeoverState, error)
	// RestoreSystemDNS undoes the takeover described by state.
	RestoreSystemDNS(state *TakeoverState) error
	// FlushDNSCache drops cached OS resolver entries so poisoned answers do
	// not survive the takeover.
	FlushDNSCache() error
	// DiscoverSystemDNS returns plaintext upstreams for non-polluted domains:
	// pre-takeover values from saved state when available, otherwise the live
	// OS configuration. Loopback addresses are filtered out to avoid query
	// loops, and public fallbacks are used when nothing is found.
	DiscoverSystemDNS() []string
	// DescribeState renders a human summary of a takeover state.
	DescribeState(state *TakeoverState) string
}

// current is set by the per-OS implementation files (platform_windows.go,
// platform_linux.go, ...).
var current Platform

// Current returns the platform implementation for this OS.
func Current() Platform { return current }

// StateFilePath is where the takeover state is persisted.
func StateFilePath() string {
	return filepath.Join(paths.StateDir(), "takeover-state.json")
}

// SaveState persists a takeover state atomically.
func SaveState(state *TakeoverState) error {
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := StateFilePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, StateFilePath())
}

// LoadState reads the persisted takeover state, validating the platform.
func LoadState() (*TakeoverState, error) {
	data, err := os.ReadFile(StateFilePath())
	if err != nil {
		return nil, err
	}
	var s TakeoverState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", StateFilePath(), err)
	}
	if s.Platform != "" && s.Platform != Current().Name() {
		return nil, fmt.Errorf("state file was created on %q, but this system is %q", s.Platform, Current().Name())
	}
	return &s, nil
}

// ClearState removes the persisted takeover state; a missing file is fine.
func ClearState() error {
	if err := os.Remove(StateFilePath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// StateSummary reports whether a takeover is currently recorded and, if so,
// its description.
func StateSummary() (bool, string, error) {
	st, err := LoadState()
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	return true, Current().DescribeState(st), nil
}

// newState builds a TakeoverState for the current platform.
func newState(payload any) (*TakeoverState, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &TakeoverState{
		Tool:     "truedns",
		Version:  version.Version,
		Platform: Current().Name(),
		TakenAt:  time.Now(),
		Payload:  data,
	}, nil
}

// DefaultFallbackServers are used when no usable system DNS server can be
// discovered (post-takeover every live value is the loopback proxy, which
// must never be used as an upstream).
var DefaultFallbackServers = []string{"223.5.5.5", "119.29.29.29", "1.1.1.1"}

// normalizeAddrs dedupes DNS server addresses, drops loopback/unspecified/
// malformed entries and appends port 53 when missing.
func normalizeAddrs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		host := a
		if h, _, err := net.SplitHostPort(a); err == nil {
			host = h
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		withPort, err := addPort(a)
		if err != nil {
			continue
		}
		if seen[withPort] {
			continue
		}
		seen[withPort] = true
		out = append(out, withPort)
	}
	return out
}

// addPort appends :53 to a DNS server address that lacks a port.
func addPort(addr string) (string, error) {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr, nil
	}
	if strings.HasPrefix(addr, "[") || strings.Contains(addr, ":") {
		ip := net.ParseIP(strings.Trim(addr, "[]"))
		if ip == nil {
			return "", fmt.Errorf("invalid address %q", addr)
		}
		return net.JoinHostPort(ip.String(), "53"), nil
	}
	if net.ParseIP(addr) == nil {
		return "", fmt.Errorf("invalid address %q", addr)
	}
	return net.JoinHostPort(addr, "53"), nil
}
