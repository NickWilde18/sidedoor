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

// TestConfigCandidatesForRoot 钉住「sudo 下改到普通用户的配置」：macOS 的 sudo 会保留
// HOME，root 要是也先找 ~/.config/sidedoor，sudo sidedoor uninstall 就会以 root 身份
// 删掉普通模式的配置，而不是 tun 模式的。root 只该看系统级配置。
func TestConfigCandidatesForRoot(t *testing.T) {
	if got := configCandidates(true); !slices.Equal(got, []string{configDir(true)}) {
		t.Errorf("root 查找配置的目录 %q，应当只有系统级的 %q", got, configDir(true))
	}
	// 普通用户先找自己的，再找系统级的：status 发现是 tun 模式时好提示加 sudo。
	if got := configCandidates(false); !slices.Equal(got, []string{configDir(false), configDir(true)}) {
		t.Errorf("普通用户查找配置的目录 %q", got)
	}
}
