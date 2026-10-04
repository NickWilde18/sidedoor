package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// TestFindConfigTunNeedsSudo 钉住「普通用户直接 sidedoor uninstall，只报 permission denied」：
// 自己的目录里没有配置，就去读 tun 模式的系统级配置；那个文件只有 root 能读，读的时候报
// 权限错误，findConfig 原样返回，提示加 sudo 的 requireRoot 根本走不到。
func TestFindConfigTunNeedsSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 读得了任何文件，测不出权限错误")
	}
	t.Setenv("HOME", t.TempDir()) // 普通模式的配置目录里没有配置
	defer func(dir string) { systemConfigDir = dir }(systemConfigDir)
	systemConfigDir = t.TempDir()
	path := filepath.Join(systemConfigDir, "config.json")
	if err := os.WriteFile(path, []byte(`{"mode":"tun"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 模拟 root 的 0600 文件：当前用户读不了。
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)

	_, _, err := findConfig()
	if err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("tun 模式的配置读不了时应当提示加 sudo，实际 %v", err)
	}
}
