package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kardianos/service"
)

// serviceName 是 launchd 任务的标签，也是 plist 和日志的文件名。
const serviceName = "com.nickwilde.sidedoor"

// systemConfigDir 是 tun 模式的配置目录。测试里会换成临时目录。
var systemConfigDir = "/Library/Application Support/sidedoor"

func userConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sidedoor")
}

func logDir(root bool) string {
	if root {
		return "/Library/Logs/sidedoor"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "sidedoor")
}

// serviceFile 是后台服务的 launchd 任务文件。
func serviceFile(cfg *Config) string {
	if cfg.Mode == ModeTUN {
		return "/Library/LaunchDaemons/" + serviceName + ".plist"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", serviceName+".plist")
}

// serviceOptions 描述后台服务。tun 模式要建网卡，只能是 root 的系统级任务
// （/Library/LaunchDaemons）；其余模式是当前用户的 LaunchAgent，登录后自动启动。
// 进程退出后 launchd 会自动拉起（KeepAlive），断线重连由 sing-box 自己负责。
func serviceOptions(cfg *Config) service.KeyValue {
	return service.KeyValue{
		"UserService":  cfg.Mode != ModeTUN,
		"RunAtLoad":    true,
		"KeepAlive":    true,
		"LogOutput":    true,
		"LogDirectory": logDir(cfg.Mode == ModeTUN),
	}
}

// serviceNote 在装好服务之后，说明它什么时候启动、挂了怎么办。
func serviceNote(*Config) {
	note("登录系统后自动启动；进程意外退出时 launchd 会自动把它拉起来")
}

// useKeychain 说明密码是不是存在系统钥匙串里。tun 模式例外，见 passwordFile。
func useKeychain(cfg *Config) bool { return cfg.Mode != ModeTUN }

// modeUnsupported 说明这个系统为什么用不了 mode；用得了时返回空。
func modeUnsupported(string) string { return "" }

func systemProxySettings() (map[string]string, bool) {
	out, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		return nil, false
	}
	return parseScutil(out), true
}

func openBrowser(url string) error { return exec.Command("open", url).Run() }

// asUserCommand 让 root 以 uid / name 这个用户的身份运行 cmd：launchctl asuser 进他的登录会话
// （LaunchAgent 和钥匙串都在会话里），sudo -H 换成他的身份和 HOME。
func asUserCommand(uid, name string, cmd []string) []string {
	return append([]string{"launchctl", "asuser", uid, "sudo", "-H", "-u", name}, cmd...)
}
