package main

import (
	"slices"
	"testing"
)

func TestNormalizeServer(t *testing.T) {
	cases := map[string]string{
		"vpn.example.edu":                     "vpn.example.edu",
		"  VPN.Example.edu  ":                 "vpn.example.edu",
		"https://vpn.example.edu/":            "vpn.example.edu",
		"https://vpn.example.edu/portal?x=1":  "vpn.example.edu",
		"vpn.example.edu:8443":                "vpn.example.edu:8443",
		"https://vpn.example.edu:8443/portal": "vpn.example.edu:8443",
	}
	for input, want := range cases {
		got, err := normalizeServer(input)
		if err != nil || got != want {
			t.Errorf("normalizeServer(%q) = %q, %v；想要 %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "https://", "   "} {
		if got, err := normalizeServer(input); err == nil {
			t.Errorf("normalizeServer(%q) = %q，应当报错", input, got)
		}
	}
}

func TestDefaultDomains(t *testing.T) {
	cases := map[string][]string{
		"vpn.example.edu":      {"example.edu"},
		"vpn.example.edu:8443": {"example.edu"},
		"vpn.dept.example.edu": {"dept.example.edu"},
		"example.edu":          {"example.edu"},
		"10.0.0.1":             nil,
		"10.0.0.1:443":         nil,
	}
	for server, want := range cases {
		if got := defaultDomains(server); !slices.Equal(got, want) {
			t.Errorf("defaultDomains(%q) = %q，想要 %q", server, got, want)
		}
	}
}

func TestParseDomains(t *testing.T) {
	got := parseDomains(" Example.EDU, .foo.edu. bar.edu,,")
	if want := []string{"example.edu", "foo.edu", "bar.edu"}; !slices.Equal(got, want) {
		t.Errorf("parseDomains = %q，想要 %q", got, want)
	}
}
