package platform

import "testing"

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
