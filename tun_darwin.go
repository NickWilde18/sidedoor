//go:build darwin

package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	resolverDir    = "/etc/resolver"
	resolverMarker = "# sidedoor 生成，停止时删除"
)

// tunUp 做 tun 模式在 sing-box 之外的两件事：
//
//  1. 确认假 IP 段没被别的网卡接管。被占就不启动，不去抢。
//  2. 在 /etc/resolver 下为每个域名后缀写一份分流 DNS。macOS 只会把这些域名的
//     查询发给我们的网卡，其余域名的 DNS 设置一概不动。
func tunUp(cfg *Config) error {
	if out, err := exec.Command("route", "-n", "get", fakeIPProbe).Output(); err == nil {
		if owner, taken := routeConflict(out); taken {
			return fmt.Errorf("假 IP 段 %s 已被 %s 接管，tun 模式不去抢它", fakeIPRange, owner)
		}
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		return err
	}
	for _, domain := range cfg.Domains {
		path := filepath.Join(resolverDir, domain)
		if data, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(data), resolverMarker) {
			return fmt.Errorf("%s 已存在，而且不是 sidedoor 写的，不覆盖", path)
		}
		content := resolverMarker + "\nnameserver " + tunDNS + "\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// tunDown 删掉 tunUp 写的分流 DNS 文件。只删带自己标记的，别人的不碰。
func tunDown(cfg *Config) {
	for _, domain := range cfg.Domains {
		path := filepath.Join(resolverDir, domain)
		data, err := os.ReadFile(path)
		if err != nil || !strings.HasPrefix(string(data), resolverMarker) {
			continue
		}
		if err := os.Remove(path); err != nil {
			log.Printf("删除 %s 失败: %v", path, err)
		}
	}
}

// routeConflict 根据 route -n get 的输出判断假 IP 段是否已被别的网卡专门接管。
//
// 只算比假 IP 段更具体、或者一样具体的路由。很多 VPN 用 0.0.0.0/1 加 128.0.0.0/1
// 两条路由盖住默认路由，它们按最长前缀匹配会输给我们的 /24，不算冲突。
func routeConflict(out []byte) (string, bool) {
	var destination, mask, iface string
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch value = strings.TrimSpace(value); key {
		case "destination":
			destination = value
		case "mask":
			mask = value
		case "interface":
			iface = value
		}
	}
	if destination == "" || destination == "default" || mask == "default" {
		return "", false
	}
	bits := 32 // 主机路由没有 mask 这一行
	if mask != "" {
		ip := net.ParseIP(mask).To4()
		if ip == nil {
			return "", false
		}
		bits, _ = net.IPMask(ip).Size()
	}
	if bits < 24 {
		return "", false
	}
	return fmt.Sprintf("%s/%d（%s）", destination, bits, iface), true
}
