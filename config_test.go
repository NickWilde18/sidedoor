package main

import (
	"os"
	"os/user"
	"path/filepath"
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

// TestSideDirForRoot 钉住「sudo 下改到普通用户的配置」：macOS 的 sudo 会保留 HOME，root 要是
// 按 HOME 找普通模式的配置，Linux（sudo 换掉 HOME）上就找不到；按 SUDO_USER 找，两个平台一样。
//
// 条件：root 身份，HOME 指向别处，SUDO_USER 是 alice（家目录是临时目录）；再去掉 SUDO_USER。
// 期待：普通模式那一边是 alice 的 ~/.config/sidedoor，不跟着 HOME 走；没有 SUDO_USER 时是空串，
// 表示不知道是谁、不去碰任何人的配置。tun 那一边始终是系统目录。
func TestSideDirForRoot(t *testing.T) {
	defer func(f func() int) { currentEUID = f }(currentEUID)
	defer func(f func(string) (*user.User, error)) { lookupUser = f }(lookupUser)
	currentEUID = func() int { return 0 }
	aliceHome := t.TempDir()
	lookupUser = func(name string) (*user.User, error) {
		if name != "alice" {
			t.Fatalf("应当按 SUDO_USER 查用户，实际查的是 %q", name)
		}
		return &user.User{Username: name, HomeDir: aliceHome}, nil
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUDO_USER", "alice")
	if got, want := sideDir(sideUser), filepath.Join(aliceHome, ".config", "sidedoor"); got != want {
		t.Errorf("root 看普通模式那一边应当是 %s，实际 %s", want, got)
	}
	if got := sideDir(sideTUN); got != systemConfigDir {
		t.Errorf("tun 那一边应当是系统目录 %s，实际 %s", systemConfigDir, got)
	}
	t.Setenv("SUDO_USER", "")
	if got := sideDir(sideUser); got != "" {
		t.Errorf("不知道是哪个用户运行的 sudo 时应当是空串，实际 %s", got)
	}
}

// TestInstalledSidesWithUnreadableTunConfig 钉住「普通用户看不见 root 那边装没装」：旧版本的
// tun 配置只有 root 能读，判断装没装不能靠读文件。
//
// 条件：普通用户，自己的目录里没有配置；系统目录里有一份当前用户读不了的配置。
// 期待：判成 tun 那一边装着。
func TestInstalledSidesWithUnreadableTunConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 读得了任何文件，测不出权限")
	}
	t.Setenv("HOME", t.TempDir())
	defer func(dir string) { systemConfigDir = dir }(systemConfigDir)
	systemConfigDir = t.TempDir()
	path := filepath.Join(systemConfigDir, "config.json")
	if err := os.WriteFile(path, []byte(`{"mode":"tun"}`), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)
	if got := installedSides(); !slices.Equal(got, []side{sideTUN}) {
		t.Errorf("应当判成 tun 那一边装着，实际 %v", got)
	}
}
