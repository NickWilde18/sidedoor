package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
)

// TestTunConfigReadableWithoutSecret 钉住「普通用户运行 sidedoor status 看不了 tun 模式」：
// tun 模式的 config.json 是 root 的 0600，里面混着状态接口的密钥，普通用户一读就是权限错误。
//
// 条件：以 tun 模式存一份带密钥的配置。期待：config.json 是 0644、内容里没有密钥；密钥单独存进
// 同目录下 0600 的 api_secret；root 读配置时能把密钥合回来。
func TestTunConfigReadableWithoutSecret(t *testing.T) {
	defer func(dir string) { systemConfigDir = dir }(systemConfigDir)
	systemConfigDir = t.TempDir()
	path := filepath.Join(systemConfigDir, "config.json")
	cfg := &Config{Server: "vpn.example.edu", Mode: ModeTUN, Listen: defaultListen, APISecret: "s3cret-for-test"}
	if err := cfg.save(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("tun 模式的 config.json 权限应当是 0644（普通用户的 status 要读），实际 %o", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cret-for-test") {
		t.Errorf("所有人都能读的 config.json 里不该有状态接口的密钥，实际内容 %s", data)
	}

	secretPath := filepath.Join(systemConfigDir, "api_secret")
	if info, err := os.Stat(secretPath); err != nil {
		t.Errorf("密钥应当单独存进 %s：%v", secretPath, err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("api_secret 的权限应当是 0600，实际 %o", perm)
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APISecret != "s3cret-for-test" {
		t.Errorf("读配置时应当把单独存的密钥合回来，实际 %q", loaded.APISecret)
	}
}

// TestPlanSetupReplacesOtherMode 钉住「system 模式没卸掉就装上了 tun 模式」：两种模式的
// 配置在不同目录，setup 只停本模式目录里的旧服务，另一种模式的服务留着，和新服务抢同一个
// 端口，在 KeepAlive 下反复重启。
//
// 条件：分四种情况——普通用户装 tun / root 装 system，各自旧模式在自己这边或在另一边。
// 期待：先停旧模式（在它那边的身份下），再在目标身份下装新模式；装好后卸掉旧模式，装失败
// 就把旧模式重新启动。旧模式和新模式在同一边时，交给 setup 自己原有的「停旧服务」处理。
func TestPlanSetupReplacesOtherMode(t *testing.T) {
	cases := []struct {
		name      string
		own       side
		target    side
		installed []side
		want      setupPlan
	}{
		{
			name: "普通用户从 system 换成 tun", own: sideUser, target: sideTUN, installed: []side{sideUser},
			want: setupPlan{
				Stop:    []step{{Side: sideUser, Cmd: "stop", Local: true}},
				Install: step{Side: sideTUN, Cmd: "setup", Local: false},
				Cleanup: []step{{Side: sideUser, Cmd: "uninstall", Local: true}},
				Restore: []step{{Side: sideUser, Cmd: "start", Local: true}},
			},
		},
		{
			name: "普通用户从 tun 换成 system", own: sideUser, target: sideUser, installed: []side{sideTUN},
			want: setupPlan{
				Stop:    []step{{Side: sideTUN, Cmd: "stop", Local: false}},
				Install: step{Side: sideUser, Cmd: "setup", Local: true},
				Cleanup: []step{{Side: sideTUN, Cmd: "uninstall", Local: false}},
				Restore: []step{{Side: sideTUN, Cmd: "start", Local: false}},
			},
		},
		{
			name: "sudo 下从 tun 换成 system", own: sideTUN, target: sideUser, installed: []side{sideTUN},
			want: setupPlan{
				Stop:    []step{{Side: sideTUN, Cmd: "stop", Local: true}},
				Install: step{Side: sideUser, Cmd: "setup", Local: false},
				Cleanup: []step{{Side: sideTUN, Cmd: "uninstall", Local: true}},
				Restore: []step{{Side: sideTUN, Cmd: "start", Local: true}},
			},
		},
		{
			name: "普通用户重新配置 system", own: sideUser, target: sideUser, installed: []side{sideUser},
			want: setupPlan{Install: step{Side: sideUser, Cmd: "setup", Local: true}},
		},
		{
			name: "两种都装着（旧版本留下的）时换成 tun", own: sideUser, target: sideTUN, installed: []side{sideUser, sideTUN},
			want: setupPlan{
				Stop:    []step{{Side: sideUser, Cmd: "stop", Local: true}},
				Install: step{Side: sideTUN, Cmd: "setup", Local: false},
				Cleanup: []step{{Side: sideUser, Cmd: "uninstall", Local: true}},
				Restore: []step{{Side: sideUser, Cmd: "start", Local: true}},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planSetup(c.own, c.target, c.installed)
			if !slices.Equal(got.Stop, c.want.Stop) || got.Install != c.want.Install ||
				!slices.Equal(got.Cleanup, c.want.Cleanup) || !slices.Equal(got.Restore, c.want.Restore) {
				t.Errorf("计划\n  %+v\n应当是\n  %+v", got, c.want)
			}
		})
	}
}

// TestPlanControl 钉住「sudo sidedoor uninstall 只卸了 tun，system 模式还留着」：root 只看
// 系统目录，普通用户先看自己的目录，各自看不见另一边。
//
// 条件：两边各种装法下的 stop / uninstall / start / restart。期待：stop、uninstall 对每个
// 装着的模式各做一次，不在自己这边的切换身份去做；start、restart 只认唯一装着的那一种，
// 两种都装着或一个都没装时报错。
func TestPlanControl(t *testing.T) {
	both := []side{sideUser, sideTUN}
	steps, err := planControl("uninstall", sideTUN, both)
	want := []step{{Side: sideUser, Cmd: "uninstall", Local: false}, {Side: sideTUN, Cmd: "uninstall", Local: true}}
	if err != nil || !slices.Equal(steps, want) {
		t.Errorf("sudo 下 uninstall 两边都装着时应当是 %+v，实际 %+v（%v）", want, steps, err)
	}
	steps, err = planControl("stop", sideUser, []side{sideTUN})
	want = []step{{Side: sideTUN, Cmd: "stop", Local: false}}
	if err != nil || !slices.Equal(steps, want) {
		t.Errorf("普通用户 stop tun 模式应当切换到 root 去做 %+v，实际 %+v（%v）", want, steps, err)
	}
	steps, err = planControl("restart", sideUser, []side{sideUser})
	want = []step{{Side: sideUser, Cmd: "stop", Local: true}, {Side: sideUser, Cmd: "start", Local: true}}
	if err != nil || !slices.Equal(steps, want) {
		t.Errorf("restart 应当是先 stop 再 start：%+v，实际 %+v（%v）", want, steps, err)
	}
	if _, err := planControl("start", sideUser, both); err == nil || !strings.Contains(err.Error(), "uninstall") {
		t.Errorf("两种模式都装着时 start 应当报错并提示 uninstall，实际 %v", err)
	}
	if _, err := planControl("stop", sideUser, nil); err == nil || !strings.Contains(err.Error(), "setup") {
		t.Errorf("什么都没装时应当提示先 setup，实际 %v", err)
	}
}

// TestSwitchCommand 钉住切换身份时拼出的命令：普通用户经 sudo 变成 root，在同一个终端里
// 输密码；root 回到运行 sudo 的那个用户，要进他的登录会话（钥匙串、LaunchAgent、
// systemctl --user 都认会话），而且要用他的 HOME（macOS 的 sudo 会保留 root 的 HOME）。
//
// 条件：普通用户切到 tun；root 带 SUDO_USER/SUDO_UID 切到普通模式；root 不带 SUDO_USER。
// 期待：分别是 sudo <exe> <args>、按平台进用户会话的命令、提示不要用 sudo 运行的错误。
func TestSwitchCommand(t *testing.T) {
	defer func(f func() int) { currentEUID = f }(currentEUID)

	currentEUID = func() int { return 501 }
	got, err := switchCommand(sideTUN, "/usr/local/bin/sidedoor", []string{"stop", "-side", "tun"})
	if want := []string{"sudo", "/usr/local/bin/sidedoor", "stop", "-side", "tun"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("普通用户切到 root 应当是 %q，实际 %q（%v）", want, got, err)
	}

	currentEUID = func() int { return 0 }
	t.Setenv("SUDO_USER", "alice")
	t.Setenv("SUDO_UID", "501")
	got, err = switchCommand(sideUser, "/usr/local/bin/sidedoor", []string{"uninstall", "-side", "user"})
	want := []string{"launchctl", "asuser", "501", "sudo", "-H", "-u", "alice", "/usr/local/bin/sidedoor", "uninstall", "-side", "user"}
	if runtime.GOOS == "linux" {
		want = []string{"sudo", "-H", "-u", "alice", "env", "XDG_RUNTIME_DIR=/run/user/501",
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/501/bus", "/usr/local/bin/sidedoor", "uninstall", "-side", "user"}
	}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("root 回到普通用户应当是 %q，实际 %q（%v）", want, got, err)
	}

	t.Setenv("SUDO_USER", "")
	if _, err := switchCommand(sideUser, "/usr/local/bin/sidedoor", []string{"setup"}); err == nil || !strings.Contains(err.Error(), "不要用 sudo") {
		t.Errorf("查不到是哪个用户运行的 sudo 时，应当提示不要用 sudo 运行，实际 %v", err)
	}
}

// TestServiceStateAfterStop 钉住「sidedoor stop 之后重启电脑，VPN 又自己连上了」：stop 只是
// launchctl unload，任务文件还在、带着 RunAtLoad，下次登录 launchd 照样把它拉起来。stop 之后
// 任务文件就不该在了，status 也按任务文件和状态文件判断，不靠 launchctl list（普通用户看不到
// root 的任务）。
//
// 条件：同一份配置，依次是没有任务文件、有任务文件但没有状态、有任务文件且有当前进程写的状态。
// 期待：分别判成已停止、正在启动、运行中。
func TestServiceStateAfterStop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Mode: ModeSystem, Listen: defaultListen}
	path := filepath.Join(configDir(false), "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if st, _ := serviceState(cfg, path); st != stateStopped {
		t.Errorf("没有任务文件时应当是已停止，实际 %v", st)
	}
	if err := os.MkdirAll(filepath.Dir(serviceFile(cfg)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serviceFile(cfg), []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := serviceState(cfg, path); st != stateStarting {
		t.Errorf("有任务文件、还没写出状态时应当是正在启动，实际 %v", st)
	}
	if err := writeStatus(statusPath(path), runtimeStatus{PID: os.Getpid(), State: adapter.OpenConnectStateConnected, Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if st, rs := serviceState(cfg, path); st != stateRunning || rs == nil || rs.PID != os.Getpid() {
		t.Errorf("有任务文件、有活着的进程写的状态时应当是运行中，实际 %v %+v", st, rs)
	}
}

// TestVPNConnectionsForStatus 钉住普通用户看 tun 模式的连接明细：状态接口要密钥，密钥只有
// root 能读，所以由后台服务把走 VPN 的连接写进所有人都能读的 status.json。
//
// 条件：12 条走 VPN 的连接和 1 条直连。期待：只留走 VPN 的，按传输量从大到小取前 10 条，
// 另外记下总数 12。
func TestVPNConnectionsForStatus(t *testing.T) {
	var conns []apiConnection
	for i := range 12 {
		c := apiConnection{Upload: int64(i), Chains: []string{"vpn"}}
		c.Metadata.Host = "h" + string(rune('a'+i)) + ".example.edu"
		conns = append(conns, c)
	}
	direct := apiConnection{Upload: 1 << 30, Chains: []string{"direct"}}
	conns = append(conns, direct)

	top, total := vpnConnections(conns)
	if total != 12 || len(top) != 10 {
		t.Fatalf("应当记下 12 条、留前 10 条，实际 %d 条、留 %d 条", total, len(top))
	}
	if top[0].Upload != 11 || top[9].Upload != 2 {
		t.Errorf("应当按传输量从大到小排，实际第一条 %d、最后一条 %d", top[0].Upload, top[9].Upload)
	}
	for _, c := range top {
		if !slices.Contains(c.Chains, "vpn") {
			t.Errorf("直连的连接不该写进来：%+v", c)
		}
	}
}

// TestRestoreOnlyWhatWasRunning 钉住「换模式没装成，把用户自己停掉的旧模式也拉了起来」：setup
// 失败后的恢复步骤是给「本来在跑、被 setup 停掉」的旧模式准备的；旧模式本来就停着的话，
// 恢复它等于替用户改了主意。
//
// 条件：两边各有一个恢复步骤，只有 tun 那边在换模式之前是运行中。期待：只留 tun 那一步。
func TestRestoreOnlyWhatWasRunning(t *testing.T) {
	restore := []step{{Side: sideUser, Cmd: "start", Local: true}, {Side: sideTUN, Cmd: "start", Local: false}}
	got := onlyRunning(restore, func(s side) bool { return s == sideTUN })
	if want := []step{{Side: sideTUN, Cmd: "start", Local: false}}; !slices.Equal(got, want) {
		t.Errorf("只该恢复换模式之前在跑的 %+v，实际 %+v", want, got)
	}
}

// TestUpgradeLegacyTunConfig 确认旧版本写的 tun 配置会被 root 改成新格式：旧文件是 0600、
// 密钥混在里面，不改的话普通用户的 status 一直读不了。
//
// 条件：root 身份，系统目录里有一份 0600、带密钥的旧配置。期待：重写成 0644、不含密钥，密钥挪进
// api_secret，读回来密钥不变；普通用户身份下什么都不改。
func TestUpgradeLegacyTunConfig(t *testing.T) {
	defer func(f func() int) { currentEUID = f }(currentEUID)
	defer func(dir string) { systemConfigDir = dir }(systemConfigDir)
	systemConfigDir = t.TempDir()
	path := filepath.Join(systemConfigDir, "config.json")
	legacy := `{"server":"vpn.example.edu","mode":"tun","listen":"127.0.0.1:11080","api":"127.0.0.1:11081","api_secret":"old-secret"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	currentEUID = func() int { return 501 }
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := upgradeTunConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("普通用户身份下不该改配置，实际权限变成了 %o", info.Mode().Perm())
	}

	currentEUID = func() int { return 0 }
	if err := upgradeTunConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 || strings.Contains(string(data), "old-secret") {
		t.Errorf("root 应当把旧配置改成 0644、不含密钥，实际权限 %o、内容 %s", info.Mode().Perm(), data)
	}
	if again, err := loadConfig(path); err != nil || again.APISecret != "old-secret" {
		t.Errorf("迁移后密钥应当不变，实际 %v（%v）", again, err)
	}
}
