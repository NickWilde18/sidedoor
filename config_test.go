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

// TestFindConfigWarnsAboutLeftoverTunConfig 钉住「system 模式改成 tun 模式之后，sidedoor status
// 报后台服务已停止」：sudo sidedoor setup -mode tun 只管系统目录，~/.config/sidedoor 里
// 原来那份配置留着。普通用户运行的命令先找到的是这份旧配置，报的是它那个（一直抢不到端口、
// 反复重启的）后台服务，看不出真正在跑的是 tun 模式。
//
// 条件：两个目录各有一份配置。期待：仍然用自己目录里那份（sidedoor uninstall 要靠它卸掉
// 旧服务），但要打出警告，点出系统目录那份的路径，并给出 sudo sidedoor status 和
// sidedoor uninstall 两条出路。
func TestFindConfigWarnsAboutLeftoverTunConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 只看系统目录，测的是普通用户")
	}
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(configDir(false), 0o755); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(configDir(false), "config.json")
	if err := os.WriteFile(userPath, []byte(`{"mode":"system"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func(dir string) { systemConfigDir = dir }(systemConfigDir)
	systemConfigDir = t.TempDir()
	tunPath := filepath.Join(systemConfigDir, "config.json")
	if err := os.WriteFile(tunPath, []byte(`{"mode":"tun"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var path string
	out := captureStdout(t, func() {
		var err error
		if path, _, err = findConfig(); err != nil {
			t.Errorf("两份配置都在时不该报错（uninstall 要靠它卸掉旧服务），实际 %v", err)
		}
	})
	if path != userPath {
		t.Errorf("应当仍然用自己目录里的 %s，实际 %s", userPath, path)
	}
	for _, want := range []string{tunPath, "sudo sidedoor status", "sidedoor uninstall"} {
		if !strings.Contains(out, want) {
			t.Errorf("警告里应当有 %q，实际输出 %q", want, out)
		}
	}
}

// TestLeftoverConfig 钉住 setup 拦截的判据：普通用户看系统目录里有没有 tun 模式的配置。
//
// 条件：系统目录先空着，再放一份配置（权限 0600，模拟 root 的文件：只 stat、不读）。
// 期待：空着时返回空串；有配置时返回它的路径。
func TestLeftoverConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 看的是 SUDO_USER 的目录，测的是普通用户")
	}
	defer func(dir string) { systemConfigDir = dir }(systemConfigDir)
	systemConfigDir = t.TempDir()
	if got := leftoverConfig(false); got != "" {
		t.Errorf("系统目录里没有配置时应当返回空串，实际 %q", got)
	}
	tunPath := filepath.Join(systemConfigDir, "config.json")
	if err := os.WriteFile(tunPath, []byte(`{"mode":"tun"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := leftoverConfig(false); got != tunPath {
		t.Errorf("系统目录里有配置时应当返回 %s，实际 %q", tunPath, got)
	}
}

// TestLeftoverConfigError 钉住 setup 拒绝装第二种模式时给的出路：另一种模式的服务会和新服务
// 抢同一个端口，在 launchd 的 KeepAlive 下一直重启；开机时谁先抢到端口跑的就是谁。
//
// 条件：分别以普通用户和 root 的身份给出一份遗留配置。期待：错误里有遗留配置的路径，以及
// 卸掉它的那条命令——普通用户要卸的是 tun 模式（sudo sidedoor uninstall），root 要卸的是
// 普通模式（不加 sudo 的 sidedoor uninstall）。没有遗留配置时不报错。
func TestLeftoverConfigError(t *testing.T) {
	if err := leftoverConfigError(false, ""); err != nil {
		t.Errorf("没有遗留配置时不该报错，实际 %v", err)
	}
	err := leftoverConfigError(false, "/sys/sidedoor/config.json")
	if err == nil || !strings.Contains(err.Error(), "/sys/sidedoor/config.json") || !strings.Contains(err.Error(), "sudo sidedoor uninstall") {
		t.Errorf("普通用户遇到 tun 模式的遗留配置，应当提示 sudo sidedoor uninstall，实际 %v", err)
	}
	err = leftoverConfigError(true, "/home/alice/.config/sidedoor/config.json")
	if err == nil || !strings.Contains(err.Error(), "/home/alice/.config/sidedoor/config.json") || !strings.Contains(err.Error(), "不加 sudo 运行 sidedoor uninstall") {
		t.Errorf("root 遇到普通模式的遗留配置，应当提示不加 sudo 卸掉，实际 %v", err)
	}
}
