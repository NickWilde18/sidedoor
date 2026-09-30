package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

// Config 是 sidedoor 唯一落盘的配置。
//
// 这里只存稳定的标识：服务器存主机名而不是 URL，认证组存组名。认证过程中的表单
// 地址、会话 cookie 都只在运行时存在于内存里。密码不在这里，见 savePassword。
type Config struct {
	Server   string   `json:"server"` // VPN 服务器主机名，可带端口
	Username string   `json:"username"`
	Group    string   `json:"group,omitempty"` // 认证组；空表示用服务器默认的组
	Domains  []string `json:"domains"`         // 走 VPN 的域名后缀
	Listen   string   `json:"listen"`          // 本地代理的监听地址
	Mode     string   `json:"mode"`            // proxy、system 或 tun
}

const (
	ModeProxy  = "proxy"
	ModeSystem = "system"
	ModeTUN    = "tun"

	defaultListen  = "127.0.0.1:11080"
	keyringService = "sidedoor"
)

// configDir 返回配置目录。
//
// tun 模式的服务以 root 身份运行，配置放在系统目录，让 root 进程和普通用户的
// sidedoor status 都能找到；其余模式放在当前用户的 ~/.config 下。
func configDir(root bool) string {
	if root {
		return "/Library/Application Support/sidedoor"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sidedoor")
}

// findConfig 先找当前用户的配置，再找 tun 模式的系统级配置。
func findConfig() (string, *Config, error) {
	for _, root := range []bool{false, true} {
		path := filepath.Join(configDir(root), "config.json")
		cfg, err := loadConfig(path)
		if err == nil {
			return path, cfg, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("还没有配置，先运行 sidedoor setup")
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// keyringAccount 是密码在钥匙串里的账户名。带上服务器，换服务器时不会串号。
func (c *Config) keyringAccount() string {
	return c.Username + "@" + c.Server
}

// passwordFile 是 tun 模式存密码的文件。
//
// 普通模式的密码放在当前用户的钥匙串里。tun 模式的服务以 root 身份开机就运行，
// 那时用户还没登录，读不到用户的钥匙串；而 macOS 的 sudo 会保留 HOME，sudo setup
// 时写进去的又会落到用户的钥匙串里，两边对不上。所以 tun 模式改存成只有 root
// 能读的文件。
func passwordFile() string {
	return filepath.Join(configDir(true), "password")
}

func (c *Config) savePassword(pw string) error {
	if c.Mode == ModeTUN {
		return os.WriteFile(passwordFile(), []byte(pw), 0o600)
	}
	return keyring.Set(keyringService, c.keyringAccount(), pw)
}

func (c *Config) password() (string, error) {
	if c.Mode == ModeTUN {
		pw, err := os.ReadFile(passwordFile())
		if err != nil {
			return "", fmt.Errorf("读不到密码，重新运行 sudo sidedoor setup -mode tun: %w", err)
		}
		return string(pw), nil
	}
	pw, err := keyring.Get(keyringService, c.keyringAccount())
	if err != nil {
		return "", fmt.Errorf("钥匙串里没有 %s 的密码，重新运行 sidedoor setup: %w", c.keyringAccount(), err)
	}
	return pw, nil
}

func (c *Config) deletePassword() error {
	if c.Mode == ModeTUN {
		if err := os.Remove(passwordFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := keyring.Delete(keyringService, c.keyringAccount()); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// normalizeServer 把用户输入的服务器地址规范成主机名（可带端口）。
//
// 用户常会直接粘贴浏览器里的 URL。这里去掉协议和路径，只留主机部分：配置里
// 不存 URL，协议由 sing-box 在运行时补上。
func normalizeServer(input string) (string, error) {
	s := strings.TrimSpace(input)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("看不懂服务器地址 %q", input)
	}
	return strings.ToLower(u.Host), nil
}

// defaultDomains 从服务器主机名推出默认走 VPN 的域名后缀：去掉最左边一级。
//
// VPN 服务器通常是学校主域名下的一个子域名（vpn.example.edu → example.edu），
// 所以多数时候猜得对。setup 只把它当默认值，让用户确认或修改。
func defaultDomains(server string) []string {
	host := server
	if h, _, err := net.SplitHostPort(server); err == nil {
		host = h
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	labels := strings.Split(host, ".")
	if len(labels) >= 3 {
		labels = labels[1:]
	}
	return []string{strings.Join(labels, ".")}
}

// parseDomains 解析逗号或空格分隔的域名后缀，顺手去掉首尾的点并转小写。
func parseDomains(s string) []string {
	var out []string
	for _, d := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if d = strings.Trim(strings.ToLower(d), "."); d != "" {
			out = append(out, d)
		}
	}
	return out
}
