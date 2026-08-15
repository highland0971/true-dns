package override

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleHosts = `
# GitHub520 Host Start
140.82.114.25                 alive.github.com
20.205.243.168                api.github.com
20.205.243.166                github.com
185.199.111.133               avatars.githubusercontent.com camo.githubusercontent.com
# IPv6
2606:50c0:8000::154           ipv6.example.org
# invalid lines must be skipped
not-an-ip                     bad.example.org
no-hostname-line
1.2.3.4
1.2.3.5 extra.example.org # trailing comment
# GitHub520 Host End
`

func TestParseHosts(t *testing.T) {
	tbl := New()
	added, err := tbl.loadSource("test", strings.NewReader(sampleHosts))
	if err != nil {
		t.Fatal(err)
	}
	if added != 7 {
		t.Fatalf("added = %d, want 7", added)
	}
	cases := map[string]string{
		"alive.github.com":              "140.82.114.25",
		"api.github.com":                "20.205.243.168",
		"github.com":                    "20.205.243.166",
		"avatars.githubusercontent.com": "185.199.111.133",
		"camo.githubusercontent.com":    "185.199.111.133",
		"ipv6.example.org":              "2606:50c0:8000::154",
		"extra.example.org":             "1.2.3.5",
	}
	for name, want := range cases {
		ips := tbl.Lookup(name)
		if len(ips) != 1 || ips[0].String() != want {
			t.Errorf("Lookup(%q) = %v, want [%s]", name, ips, want)
		}
	}
	if ips := tbl.Lookup("not-in-table.example"); len(ips) != 0 {
		t.Errorf("unexpected override for unknown name: %v", ips)
	}
}

func TestLookupCaseAndDot(t *testing.T) {
	tbl := New()
	if _, err := tbl.loadSource("test", strings.NewReader("1.2.3.4 Example.COM.\n")); err != nil {
		t.Fatal(err)
	}
	ips := tbl.Lookup("example.com.")
	if len(ips) != 1 || ips[0].String() != "1.2.3.4" {
		t.Fatalf("Lookup = %v", ips)
	}
}

func TestDuplicateIPNotRepeated(t *testing.T) {
	tbl := New()
	if _, err := tbl.loadSource("test", strings.NewReader("1.2.3.4 example.org\n1.2.3.4 example.org\n")); err != nil {
		t.Fatal(err)
	}
	if ips := tbl.Lookup("example.org"); len(ips) != 1 {
		t.Fatalf("dup ips = %v", ips)
	}
}

func TestRefreshReplacesSource(t *testing.T) {
	tbl := New()
	if _, err := tbl.loadSource("sub", strings.NewReader("1.1.1.1 example.org\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := tbl.loadSource("sub", strings.NewReader("2.2.2.2 example.org\n")); err != nil {
		t.Fatal(err)
	}
	ips := tbl.Lookup("example.org")
	if len(ips) != 1 || ips[0].String() != "2.2.2.2" {
		t.Fatalf("stale IP retained after refresh: %v", ips)
	}
	meta := tbl.Meta()
	if meta.Entries != 1 || len(meta.Sources) != 1 {
		t.Fatalf("meta after refresh = %+v", meta)
	}
}

func TestMultiSourceMerge(t *testing.T) {
	tbl := New()
	_, _ = tbl.loadSource("a", strings.NewReader("1.1.1.1 example.org\n"))
	_, _ = tbl.loadSource("b", strings.NewReader("2.2.2.2 example.org\n"))
	ips := tbl.Lookup("example.org")
	if len(ips) != 2 {
		t.Fatalf("merged ips = %v", ips)
	}
	// Replacing source a must only drop its own entry.
	_, _ = tbl.loadSource("a", strings.NewReader("3.3.3.3 example.org\n"))
	ips = tbl.Lookup("example.org")
	if len(ips) != 2 {
		t.Fatalf("ips after partial refresh = %v", ips)
	}
	found := map[string]bool{}
	for _, ip := range ips {
		found[ip.String()] = true
	}
	if !found["3.3.3.3"] || !found["2.2.2.2"] {
		t.Fatalf("unexpected merge result: %v", ips)
	}
}

func TestLoadFiles(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.hosts")
	if err := os.WriteFile(f1, []byte("10.0.0.1 a.example.org\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f2 := filepath.Join(dir, "missing.hosts")
	tbl := New()
	if err := tbl.LoadFiles([]string{f1, f2}); err == nil {
		t.Fatal("expected error for missing file")
	}
	if ips := tbl.Lookup("a.example.org"); len(ips) != 1 || ips[0].String() != "10.0.0.1" {
		t.Fatalf("file override not applied: %v", ips)
	}
	meta := tbl.Meta()
	if meta.Entries != 1 || len(meta.Sources) != 1 {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestLoadURLs(t *testing.T) {
	srv := newHostsServer(t, sampleHosts)
	tbl := New()
	client := srv.client()
	if err := tbl.LoadURLs(t.Context(), []string{srv.URL}, "", client); err != nil {
		t.Fatal(err)
	}
	if ips := tbl.Lookup("github.com"); len(ips) != 1 || ips[0].String() != "20.205.243.166" {
		t.Fatalf("url override not applied: %v", ips)
	}
	if ips := tbl.Lookup("ipv6.example.org"); len(ips) != 1 || ips[0].To16() == nil {
		t.Fatalf("ipv6 override missing: %v", ips)
	}
}

func TestLoadURLsError(t *testing.T) {
	tbl := New()
	err := tbl.LoadURLs(t.Context(), []string{"http://127.0.0.1:1/none"}, "", httpClient())
	if err == nil {
		t.Fatal("expected fetch error")
	}
	if tbl.Meta().LastError == "" {
		t.Fatal("last error not recorded")
	}
}

var _ = net.ParseIP // keep net import used across test variants
