package main

import (
	"strings"
	"testing"
)

func overlayTestConfig() *Config {
	return &Config{
		Server:  "vpn.example.edu:8443",
		Domains: []string{"example.edu", "foo.edu"},
		Overlay: "127.0.0.1:11082",
	}
}

// TestOverlayRules 钉住规则的顺序：VPN 服务器的域名也在学校后缀下面，必须先让它直连，
// 不然 sidedoor 自己连 VPN 的流量会被代理软件送回 sidedoor。服务器地址里的端口要去掉。
func TestOverlayRules(t *testing.T) {
	got := overlayRules(overlayTestConfig())
	want := []string{"DOMAIN,vpn.example.edu,DIRECT", "DOMAIN-SUFFIX,example.edu,sidedoor", "DOMAIN-SUFFIX,foo.edu,sidedoor"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("叠加层规则 %q，想要 %q", got, want)
	}
}

// TestOverlayFiles 确认三种格式都写进了节点地址和完整的规则，顺序和 overlayRules 一致。
func TestOverlayFiles(t *testing.T) {
	files, err := overlayFiles(overlayTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, f := range files {
		byName[f.name] = f.content
	}
	rules := overlayRules(overlayTestConfig())
	checks := map[string][]string{
		// 规则模块不能带节点，只有规则；节点由用户在代理软件里手动加。
		"overlay.module": append([]string{"#!name=sidedoor", "[Rule]"}, rules...),
		"overlay.js":     {`"name":"sidedoor"`, `"type":"socks5"`, `"server":"127.0.0.1"`, `"port":11082`, `.concat([node])`, `["DOMAIN,vpn.example.edu,DIRECT","DOMAIN-SUFFIX,example.edu,sidedoor","DOMAIN-SUFFIX,foo.edu,sidedoor"].concat(config.rules || [])`},
		"overlay.yaml":   append([]string{"{name: sidedoor, type: socks5, server: 127.0.0.1, port: 11082}"}, rules...),
	}
	for name, wants := range checks {
		content, ok := byName[name]
		if !ok {
			t.Errorf("没有生成 %s", name)
			continue
		}
		last := -1
		for _, w := range wants {
			i := strings.Index(content, w)
			if i < 0 {
				t.Errorf("%s 里缺了 %q：\n%s", name, w, content)
				continue
			}
			if i < last {
				t.Errorf("%s 里 %q 的位置不对，规则顺序乱了：\n%s", name, w, content)
			}
			last = i
		}
	}
	if strings.Contains(byName["overlay.module"], "socks5") {
		t.Errorf("规则模块里不该定义节点：\n%s", byName["overlay.module"])
	}
}
