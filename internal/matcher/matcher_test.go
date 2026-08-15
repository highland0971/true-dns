package matcher

import "testing"

func TestMatch(t *testing.T) {
	m, err := New([]string{"github.com", "*.github.io", "example.org."})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		want bool
	}{
		{"github.com", true},
		{"api.github.com", true},
		{"raw.githubusercontent.com.", false}, // githubusercontent.com, not github.com
		{"a.b.c.github.com", true},
		{"githubusercontent.com", false}, // not github.com's subdomain
		{"github.com.evil.org", false},   // suffix must match a full label
		{"github.io", false},             // wildcard excludes the root itself
		{"x.github.io", true},
		{"a.b.github.io", true},
		{"example.org", true},
		{"sub.example.org", true},
		{"example.org.evil.com", false},
		{"github.com.cn", false},
		{"", false},
	}
	for _, c := range cases {
		if got := m.Match(c.name); got != c.want {
			t.Errorf("Match(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMatchAll(t *testing.T) {
	m, err := New([]string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match("anything.example") {
		t.Error("expected match-all to match anything")
	}
}

func TestInvalidPatterns(t *testing.T) {
	for _, p := range []string{"a*b.com", "*a.com", "*.*", "*."} {
		if _, err := New([]string{p}); err == nil {
			t.Errorf("expected error for pattern %q", p)
		}
	}
}

func TestReset(t *testing.T) {
	m, err := New([]string{"github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match("github.com") {
		t.Fatal("setup failed")
	}
	if err := m.Reset([]string{"gitlab.com"}); err != nil {
		t.Fatal(err)
	}
	if m.Match("github.com") {
		t.Error("old pattern still matches after Reset")
	}
	if !m.Match("gitlab.com") {
		t.Error("new pattern does not match")
	}
}
