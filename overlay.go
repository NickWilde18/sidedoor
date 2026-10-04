package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kardianos/service"
)

// 叠加层：在代理软件自己的订阅之上叠加一个节点和几条规则，把走 VPN 的域名转给 sidedoor
// 的叠加层专用端口。只要在代理软件里导入一次，订阅怎么更新都不受影响，订阅本身一行不改。
// 各个代理软件在哪里导入，见 README。

// overlayNode 是叠加层里那个节点的名字，规则按这个名字引用它。
const overlayNode = "sidedoor"

type overlayFile struct {
	name, kind, content string
}

// overlayRules 是要排在代理软件所有规则之前的规则，顺序不能反：VPN 服务器的域名也在
// 学校后缀下面，不先让它直连，sidedoor 自己连 VPN 的流量就会被送回 sidedoor。
func overlayRules(cfg *Config) []string {
	rules := []string{"DOMAIN," + cfg.serverHost() + ",DIRECT"}
	for _, d := range cfg.Domains {
		rules = append(rules, "DOMAIN-SUFFIX,"+d+","+overlayNode)
	}
	return rules
}

// overlayFiles 生成三种格式的叠加层，内容都来自当前配置。
func overlayFiles(cfg *Config) ([]overlayFile, error) {
	host, portText, err := net.SplitHostPort(cfg.Overlay)
	if err != nil {
		return nil, fmt.Errorf("叠加层端口 %q 不对: %w", cfg.Overlay, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("叠加层端口 %q 不对", portText)
	}
	rules := overlayRules(cfg)
	desc := "走 VPN 的域名交给 sidedoor（由 sidedoor overlay 生成）"

	var module strings.Builder
	fmt.Fprintf(&module, "#!name=%s\n#!desc=%s\n[Rule]\n", overlayNode, desc)
	for _, r := range rules {
		module.WriteString(r + "\n")
	}

	// 用 JSON 写字面量，引号和转义一定合法。
	node, err := json.Marshal(struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Server string `json:"server"`
		Port   int    `json:"port"`
	}{overlayNode, "socks5", host, port})
	if err != nil {
		return nil, err
	}
	ruleList, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	script := fmt.Sprintf(`// %s
// 节点加在列表末尾，免得成了手动选择组的默认项；规则插在最前面。
function main(config) {
  var node = %s;
  config.proxies = (config.proxies || []).filter(function (p) { return p.name !== node.name; }).concat([node]);
  config.rules = %s.concat(config.rules || []);
  return config;
}
`, desc, node, ruleList)

	quotedDesc, err := json.Marshal(desc)
	if err != nil {
		return nil, err
	}
	var yaml strings.Builder
	fmt.Fprintf(&yaml, "name: %s\ndesc: %s\nproxies:\n  - {name: %s, type: socks5, server: %s, port: %d}\nrules:\n",
		overlayNode, quotedDesc, overlayNode, host, port)
	for _, r := range rules {
		fmt.Fprintf(&yaml, "  - %s\n", r)
	}

	return []overlayFile{
		{"overlay.module", "规则模块：只有规则，节点要在代理软件里手动加一个", module.String()},
		{"overlay.js", "main(config) 脚本：节点和规则一起加", script},
		{"overlay.yaml", "YAML 覆写：节点和规则一起加", yaml.String()},
	}, nil
}

func overlayDir(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "overlays")
}

// writeOverlayFiles 把叠加层文件写到配置目录下，返回写了哪些。
func writeOverlayFiles(cfgPath string, cfg *Config) ([]overlayFile, error) {
	files, err := overlayFiles(cfg)
	if err != nil {
		return nil, err
	}
	dir := overlayDir(cfgPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.content), 0o644); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// cmdOverlay 写出叠加层文件。第一次运行时先在配置里打开叠加层专用端口，并重启后台服务。
func cmdOverlay() error {
	path, cfg, err := findConfig()
	if err != nil {
		return err
	}
	if err := requireRoot(cfg); err != nil {
		return err
	}
	s := &steps{total: 2}
	s.next("打开叠加层专用端口")
	if cfg.Overlay != "" {
		done("之前已经打开过：%s", cfg.Overlay)
	} else {
		addr := nextPort(cfg.API)
		if err := checkPortFree(addr); err != nil {
			return err
		}
		cfg.Overlay = addr
		if err := cfg.save(path); err != nil {
			return err
		}
		done("已在配置里打开：%s，只转发走 VPN 的域名，其余一律拒绝", addr)
		svc, err := newService(cfg, path)
		if err != nil {
			return err
		}
		if st, err := svc.Status(); err == nil && st == service.StatusRunning {
			if err := service.Control(svc, "restart"); err != nil {
				return err
			}
			done("后台服务已重启")
		} else {
			note("后台服务没在运行，运行 sidedoor start 之后才生效")
		}
	}

	s.next("写出叠加层文件")
	files, err := writeOverlayFiles(path, cfg)
	if err != nil {
		return err
	}
	done("已写入 %s", overlayDir(path))
	for _, f := range files {
		note("%-15s%s", f.name, f.kind)
	}
	host, port, _ := net.SplitHostPort(cfg.Overlay)
	fmt.Println("\n完成。接下来：")
	for i, line := range []string{
		"在代理软件里导入一次对应格式的文件，订阅照常更新；各软件在哪里导入，见 README 的「和其他代理软件一起用」",
		fmt.Sprintf("用规则模块的话，还要在代理软件里手动加一个 SOCKS5 节点：地址 %s，端口 %s，名字叫 %s", host, port, overlayNode),
		"以后改了走 VPN 的域名，setup 会自动更新这些文件，再导入一次就行",
	} {
		fmt.Printf("  %d. %s\n", i+1, line)
	}
	return nil
}
