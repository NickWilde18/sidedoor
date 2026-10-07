package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"
)

// tunName 是 tun 模式的网卡名。名字固定，网卡建好之后才好对它设路由和分流 DNS。
const tunName = "sidedoor0"

// tunInbound 是 tun 模式的虚拟网卡。
//
// 路由和分流 DNS 都由 tunStarted 自己设，sing-tun 的两项自动设置都关掉：
//   - auto_route 在 Linux 上会加一整套 ip rule（按 UID、网卡、防火墙标记分流），而这里
//     只需要一条路由；
//   - dns_mode 不关的话，它会用 resolvectl 给网卡设 domain ~. 和 default-route，把整台
//     机器的 DNS 都揽过来，而这里只该接管走 VPN 的域名。
//
// 发给网卡上假 DNS 的查询，照样由 addTUN 里的 hijack-dns 规则交给 sing-box。
func tunInbound() map[string]any {
	return map[string]any{
		"type":           "tun",
		"tag":            "tun-in",
		"interface_name": tunName,
		"address":        []string{tunAddress},
		"auto_route":     false,
		"dns_mode":       "disabled",
	}
}

// tunPreflight 确认这台机器能用 tun 模式：
//
//  1. 有 ip 和 resolvectl 命令，systemd-resolved 在运行。
//  2. /etc/resolv.conf 交给了 systemd-resolved。只有这样，应用的查询才会经过 resolved，
//     按分流设置把走 VPN 的域名交给我们；也只有这样，sing-box 的 local 才会只问默认网卡
//     的 DNS，解析 VPN 服务器的名字时不会又被 resolved 转回我们自己。
//  3. 假 IP 段没被别的网卡接管。被占就不启动，不去抢。
func tunPreflight(*Config) error {
	for _, cmd := range []string{"ip", "resolvectl"} {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("tun 模式要用到 %s 命令，这台机器上没有；可以改用 proxy 模式", cmd)
		}
	}
	if err := exec.Command("resolvectl", "status").Run(); err != nil {
		return errors.New("tun 模式要用 systemd-resolved 做分流 DNS，但它没在运行；可以改用 proxy 模式")
	}
	if data, err := os.ReadFile("/etc/resolv.conf"); err != nil || !resolvedManaged(data) {
		return errors.New("tun 模式要用 systemd-resolved 做分流 DNS，但 /etc/resolv.conf 没交给它管；可以改用 proxy 模式")
	}
	if out, err := exec.Command("ip", "-j", "-4", "route", "show", "table", "all", "match", fakeIPProbe).Output(); err == nil {
		if owner, taken := routeConflict(out); taken {
			return fmt.Errorf("假 IP 段 %s 已被 %s 接管，tun 模式不去抢它", fakeIPRange, owner)
		}
	}
	return nil
}

// resolvedManaged 判断 /etc/resolv.conf 是不是由 systemd-resolved 管理：开头的注释里
// 提到 systemd-resolved。判断方法照抄 sing-box 的 local（dns/transport/local 的
// isSystemdResolvedManaged），两边必须一致，原因见 tunPreflight。
func resolvedManaged(data []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '#' {
			return false
		}
		if strings.Contains(line, "systemd-resolved") {
			return true
		}
	}
	return false
}

// routeConflict 根据 ip -j route show table all match 的输出，判断假 IP 段是否已被别的
// 网卡专门接管。和 macOS 一样，只算比假 IP 段更具体、或者一样具体的路由；默认路由和
// 0.0.0.0/1 这类大段按最长前缀匹配会输给我们的 /24，不算冲突。
func routeConflict(out []byte) (string, bool) {
	var routes []struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal(out, &routes); err != nil {
		return "", false
	}
	for _, r := range routes {
		if r.Dst == "" || r.Dst == "default" || r.Dev == tunName {
			continue
		}
		prefix, err := netip.ParsePrefix(r.Dst)
		if err != nil {
			addr, err := netip.ParseAddr(r.Dst) // 主机路由不带前缀长度
			if err != nil {
				continue
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if prefix.Bits() < 24 {
			continue
		}
		owner := r.Dst
		if r.Dev != "" {
			owner += "（" + r.Dev + "）"
		}
		return owner, true
	}
	return "", false
}

// tunUp 在启动 sing-box 之前确认这台机器能用 tun 模式。路由和分流 DNS 要等网卡建好，见 tunStarted。
func tunUp(cfg *Config) error { return tunPreflight(cfg) }

// tunStarted 在网卡建好之后给它加假 IP 段的路由，再让 systemd-resolved 只把走 VPN 的
// 域名的查询发给网卡上的假 DNS。default-route 关掉，其余域名照旧问原来的 DNS。
func tunStarted(cfg *Config) error {
	if out, err := exec.Command("ip", "route", "replace", fakeIPRange, "dev", tunName).CombinedOutput(); err != nil {
		return fmt.Errorf("给 %s 加假 IP 段的路由失败: %v: %s", tunName, err, bytes.TrimSpace(out))
	}
	domain := []string{"domain", tunName}
	for _, d := range cfg.Domains {
		domain = append(domain, "~"+d)
	}
	// 先设路由域、关掉 default-route，最后才给 DNS 地址：网卡有了 DNS 却还没有路由域的那一刻，
	// resolved 会把它当成默认 DNS，所有查询都发过来。
	steps := [][]string{domain, {"default-route", tunName, "false"}, {"dns", tunName, tunDNS}}
	// resolved 要先认出新网卡才能设置，网卡刚建好时偶尔会报 Link … not known，多试几次。
	var err error
	for range 20 {
		if err = resolvectl(steps); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("设置分流 DNS 失败: %w", err)
}

func resolvectl(steps [][]string) error {
	for _, args := range steps {
		if out, err := exec.Command("resolvectl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("resolvectl %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
		}
	}
	return nil
}

// tunCleanupText 是卸载时对 tunDown 做了什么的说明。
func tunCleanupText() string {
	return "已撤掉 " + tunName + " 网卡上的分流 DNS 和假 IP 段路由"
}

// tunDown 撤掉分流 DNS 和路由。网卡随 sing-box 关闭（或者进程意外退出）一起消失，这两样
// 也跟着没了；这里管的是网卡还在的情况，比如卸载时服务还没停干净。
func tunDown(*Config) {
	if _, err := net.InterfaceByName(tunName); err != nil {
		return
	}
	if out, err := exec.Command("resolvectl", "revert", tunName).CombinedOutput(); err != nil {
		log.Printf("撤掉 %s 的分流 DNS 失败: %v: %s", tunName, err, bytes.TrimSpace(out))
	}
	_ = exec.Command("ip", "route", "del", fakeIPRange, "dev", tunName).Run()
}
