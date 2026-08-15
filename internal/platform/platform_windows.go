//go:build windows

package platform

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func init() { current = windowsPlatform{} }

type windowsPlatform struct{}

func (windowsPlatform) Name() string { return "windows" }

const (
	ifacesKey = `SYSTEM\CurrentControlSet\Services\Tcpip\Parameters\Interfaces`
	paramsKey = `SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`
	loopback  = "127.0.0.1"
)

// IsElevated reports whether the process runs with administrator privileges.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// ElevateArgs relaunches the current executable with the given arguments
// through UAC. It returns (true, nil) when the relaunch was initiated — the
// caller should exit — and (false, nil) when the process is already elevated.
func ElevateArgs(args []string) (bool, error) {
	if IsElevated() {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(quoteArgs(args))
	// ShellExecute returns an error when the return code is <= 32, which
	// covers both failure and the user cancelling the UAC prompt.
	if err := windows.ShellExecute(0, verb, file, params, nil, windows.SW_SHOWNORMAL); err != nil {
		return false, fmt.Errorf("elevation failed: %v (the UAC prompt was cancelled or unavailable)", err)
	}
	return true, nil
}

// Elevate relaunches the current executable with the current process
// arguments through UAC.
func Elevate() (bool, error) { return ElevateArgs(os.Args[1:]) }

func quoteArgs(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		if strings.ContainsAny(a, " \t\"") {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(a, `"`, `\"`))
			b.WriteByte('"')
		} else {
			b.WriteString(a)
		}
	}
	return b.String()
}

// winAdapter records the per-interface DNS configuration before takeover so
// it can be restored byte-for-byte.
type winAdapter struct {
	Key            string `json:"key"`
	NameServer     string `json:"name_server,omitempty"`
	HadNameServer  bool   `json:"had_name_server"`
	DhcpNameServer string `json:"dhcp_name_server,omitempty"`
}

// SetSystemDNS writes NameServer=127.0.0.1 to every network interface that
// carries IP configuration. A static NameServer value takes precedence over
// the DHCP-assigned one, so this also captures DHCP interfaces.
func (windowsPlatform) SetSystemDNS() (*TakeoverState, error) {
	if !IsElevated() {
		return nil, fmt.Errorf("administrator privileges required: run from an elevated prompt, or let truedns relaunch itself elevated")
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, ifacesKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, fmt.Errorf("open Tcpip interfaces: %w", err)
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(0)
	if err != nil {
		return nil, fmt.Errorf("enumerate Tcpip interfaces: %w", err)
	}
	var touched []winAdapter
	var skipped []string
	for _, name := range names {
		ik, err := registry.OpenKey(k, name, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			skipped = append(skipped, name+"(unreadable)")
			continue // adapter key we cannot open: skip
		}
		ad := winAdapter{Key: name}
		if ns, _, err := ik.GetStringValue("NameServer"); err == nil {
			ad.NameServer, ad.HadNameServer = ns, true
		}
		if dh, _, err := ik.GetStringValue("DhcpNameServer"); err == nil {
			ad.DhcpNameServer = dh
		}
		// IPAddress is REG_MULTI_SZ, not REG_SZ.
		ips, _, _ := ik.GetStringsValue("IPAddress")
		dhcp, _, _ := ik.GetStringValue("EnableDHCP")
		if !adapterActive(dhcp, ips, ad.NameServer, ad.DhcpNameServer) {
			ik.Close()
			skipped = append(skipped, name+"(no ip config)")
			continue
		}
		if err := ik.SetStringValue("NameServer", loopback); err != nil {
			ik.Close()
			return nil, fmt.Errorf("set NameServer on adapter %s: %w", name, err)
		}
		ik.Close()
		touched = append(touched, ad)
	}
	if len(touched) == 0 {
		detail := strings.Join(skipped, "; ")
		if len(detail) > 512 {
			detail = detail[:512] + "..."
		}
		if detail == "" {
			detail = "no interface keys present"
		}
		return nil, fmt.Errorf("no active network adapters found to take over (%s)", detail)
	}
	return newState(touched)
}

// RestoreSystemDNS reverts the per-interface NameServer values recorded in
// the state. Adapters that had no static value get the value deleted so
// DHCP-assigned DNS applies again.
func (windowsPlatform) RestoreSystemDNS(state *TakeoverState) error {
	if !IsElevated() {
		return fmt.Errorf("administrator privileges required to restore system DNS")
	}
	var touched []winAdapter
	if err := json.Unmarshal(state.Payload, &touched); err != nil {
		return fmt.Errorf("bad state payload: %w", err)
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, ifacesKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return fmt.Errorf("open Tcpip interfaces: %w", err)
	}
	defer k.Close()
	for _, ad := range touched {
		ik, err := registry.OpenKey(k, ad.Key, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			continue
		}
		if ad.HadNameServer {
			if err := ik.SetStringValue("NameServer", ad.NameServer); err != nil {
				ik.Close()
				return fmt.Errorf("restore NameServer on adapter %s: %w", ad.Key, err)
			}
		} else {
			// Ignore "value does not exist": deleting is the goal.
			_ = ik.DeleteValue("NameServer")
		}
		ik.Close()
	}
	return nil
}

// FlushDNSCache clears the Windows resolver cache so poisoned entries from
// before the takeover are dropped.
func (windowsPlatform) FlushDNSCache() error {
	out, err := exec.Command("ipconfig", "/flushdns").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ipconfig /flushdns: %v: %s", err, out)
	}
	return nil
}

// DescribeState summarizes a takeover state for humans.
func (windowsPlatform) DescribeState(state *TakeoverState) string {
	var touched []winAdapter
	if json.Unmarshal(state.Payload, &touched) != nil {
		return fmt.Sprintf("windows takeover taken at %s", state.TakenAt.Format("2006-01-02 15:04:05"))
	}
	return fmt.Sprintf("windows: %d adapter(s) point to 127.0.0.1 (taken at %s)", len(touched), state.TakenAt.Format("2006-01-02 15:04:05"))
}

// DiscoverSystemDNS prefers the pre-takeover values from saved state, then
// falls back to live registry values (which are loopback post-takeover and
// thus filtered), then to public resolvers.
func (windowsPlatform) DiscoverSystemDNS() []string {
	var raw []string
	if st, err := LoadState(); err == nil {
		var ads []winAdapter
		if json.Unmarshal(st.Payload, &ads) == nil {
			for _, ad := range ads {
				raw = append(raw, splitDNSList(ad.NameServer)...)
				raw = append(raw, splitDNSList(ad.DhcpNameServer)...)
			}
			if got := normalizeAddrs(raw); len(got) > 0 {
				return got
			}
			raw = nil
		}
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, ifacesKey, registry.ENUMERATE_SUB_KEYS); err == nil {
		if names, err := k.ReadSubKeyNames(0); err == nil {
			for _, n := range names {
				if ik, err := registry.OpenKey(k, n, registry.QUERY_VALUE); err == nil {
					if v, _, err := ik.GetStringValue("NameServer"); err == nil {
						raw = append(raw, splitDNSList(v)...)
					}
					if v, _, err := ik.GetStringValue("DhcpNameServer"); err == nil {
						raw = append(raw, splitDNSList(v)...)
					}
					ik.Close()
				}
			}
		}
		k.Close()
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, paramsKey, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetStringValue("NameServer"); err == nil {
			raw = append(raw, splitDNSList(v)...)
		}
		if v, _, err := k.GetStringValue("DhcpNameServer"); err == nil {
			raw = append(raw, splitDNSList(v)...)
		}
		k.Close()
	}
	got := normalizeAddrs(raw)
	if len(got) == 0 {
		got = normalizeAddrs(DefaultFallbackServers)
	}
	return got
}

// splitDNSList splits a Windows DNS server list ("1.2.3.4, 5.6.7.8" or
// space-separated, possibly with IPv6 literals).
func splitDNSList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
}
