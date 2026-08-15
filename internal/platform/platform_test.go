package platform

import (
	"net"
	"testing"
)

func TestNormalizeAddrsFiltersSelfAndLoopback(t *testing.T) {
	// Pick one of this machine's own non-loopback IPs, if any.
	var self string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP != nil {
			if ip := ipn.IP.To4(); ip != nil && !ip.IsLoopback() {
				self = ip.String()
				break
			}
		}
	}
	if self == "" {
		t.Skip("no non-loopback local IP available")
	}
	out := normalizeAddrs([]string{self, "127.0.0.1", "8.8.8.8", "8.8.8.8"})
	if len(out) != 1 || out[0] != "8.8.8.8:53" {
		t.Fatalf("normalizeAddrs = %v, want [8.8.8.8:53]", out)
	}
}

func TestAdapterActive(t *testing.T) {
	cases := []struct {
		name                       string
		dhcp                       string
		ips                        []string
		nameServer, dhcpNameServer string
		want                       bool
	}{
		{"dhcp adapter", "1", nil, "", "", true},
		{"static ipv4", "0", []string{"192.168.1.10"}, "", "", true},
		{"static ipv6", "0", []string{"fe80::1"}, "", "", true},
		{"multisz with empty first", "0", []string{"", "192.168.1.10"}, "", "", true},
		{"no ip but has dns", "0", nil, "223.5.5.5", "", true},
		{"no ip but has dhcp dns", "0", nil, "", "223.5.5.5", true},
		{"inactive adapter", "0", nil, "", "", false},
		{"zero ip only", "0", []string{"0.0.0.0"}, "", "", false},
		{"unspecified ipv6 only", "0", []string{"::"}, "", "", false},
	}
	for _, c := range cases {
		if got := adapterActive(c.dhcp, c.ips, c.nameServer, c.dhcpNameServer); got != c.want {
			t.Errorf("%s: adapterActive(%q, %v, %q, %q) = %v, want %v",
				c.name, c.dhcp, c.ips, c.nameServer, c.dhcpNameServer, got, c.want)
		}
	}
}
