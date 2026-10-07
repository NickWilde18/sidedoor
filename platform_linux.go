package main

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kardianos/service"
)

// serviceName 是 systemd 单元的名字（sidedoor.service），也是日志的文件名。
const serviceName = "sidedoor"

// systemConfigDir 是 tun 模式的配置目录。测试里会换成临时目录。
var systemConfigDir = "/etc/sidedoor"

// 配置和日志都放在固定的位置，不看 XDG_CONFIG_HOME、XDG_STATE_HOME：systemd 起服务时
// 不带登录 shell 里设的这些变量，按它们算路径的话，setup 写的和后台服务读的会对不上。

func userConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sidedoor")
}

func logDir(root bool) string {
	if root {
		return "/var/log/sidedoor"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "sidedoor")
}

// serviceFile 是后台服务的 systemd 单元文件。
func serviceFile(cfg *Config) string {
	if cfg.Mode == ModeTUN {
		return "/etc/systemd/system/" + serviceName + ".service"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", serviceName+".service")
}

// serviceOptions 描述后台服务。tun 模式要建网卡，是 root 的系统服务；其余模式是当前
// 用户的 systemd 用户服务。单元文件用自己的模板，见 systemdUnit。
func serviceOptions(cfg *Config) service.KeyValue {
	userService := cfg.Mode != ModeTUN
	return service.KeyValue{
		"UserService":   userService,
		"LogDirectory":  logDir(!userService),
		"SystemdScript": systemdUnit(userService),
	}
}

// systemdUnit 是 systemd 单元的模板，kardianos/service 往里填可执行文件的路径和参数。
//
// 不用它自带的模板，因为那份模板：日志用 file:，每次启动都从文件开头往上覆盖；进程挂了
// 要等 120 秒才重启；用户服务也写 WantedBy=multi-user.target，而用户级的 systemd 里
// 没有这个 target，开机和登录时都不会自动启动。
func systemdUnit(userService bool) string {
	unit := `[Unit]
Description={{Description}}
ConditionFileIsExecutable={{Path | cmdEscape}}
# 进程退出后一直拉起，不因为短时间里重启了太多次就放弃。
StartLimitIntervalSec=0
`
	if !userService {
		unit += "After=network-online.target\nWants=network-online.target\n"
	}
	unit += `
[Service]
ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}
Restart=always
RestartSec=10
# 追加写：日志太大时 sidedoor 会截短它，新内容要接着写在末尾。
StandardOutput=append:{{LogDirectory}}/{{Name}}.out.log
StandardError=append:{{LogDirectory}}/{{Name}}.err.log

[Install]
`
	if userService {
		return unit + "WantedBy=default.target\n"
	}
	return unit + "WantedBy=multi-user.target\n"
}

// serviceNote 在装好服务之后，说明它什么时候启动、挂了怎么办。用户服务默认只在
// 登录期间运行，要开机就跑、退出登录也不停，得给这个用户开 linger。
func serviceNote(cfg *Config) {
	switch {
	case cfg.Mode == ModeTUN:
		note("开机自动启动；进程意外退出时 systemd 会在 10 秒后把它拉起来")
	case lingering():
		note("这个用户开启了 linger，开机就自动启动；进程意外退出时 systemd 会在 10 秒后把它拉起来")
	default:
		note("登录后自动启动；进程意外退出时 systemd 会在 10 秒后把它拉起来")
		warn("这个用户没开 linger：退出登录后服务会被停掉。想让它开机就运行、退出登录也不停，运行 sudo loginctl enable-linger %s", currentUser())
	}
}

func lingering() bool {
	out, err := exec.Command("loginctl", "show-user", strconv.Itoa(os.Getuid()), "-p", "Linger", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// useKeychain：Linux 上一律存成只有本人能读的文件。没有图形界面的服务器上没有钥匙串
// 服务；有图形界面时，开机就启动的用户服务也常常赶在钥匙串解锁之前。
func useKeychain(*Config) bool { return false }

// modeUnsupported 说明这个系统为什么用不了 mode；用得了时返回空。
func modeUnsupported(mode string) string {
	if mode == ModeSystem {
		return "Linux 上暂时只支持 proxy 和 tun 两种模式：system 模式要改桌面环境的系统代理设置，还没在 Linux 桌面上验证过"
	}
	return ""
}

func systemProxySettings() (map[string]string, bool) { return nil, false }

func openBrowser(url string) error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return errors.New("没有图形界面")
	}
	return exec.Command("xdg-open", url).Run()
}
