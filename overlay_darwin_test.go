package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestOverlayScriptRuns 真的执行一遍生成的 main(config) 脚本（macOS 自带的 JavaScript 引擎），
// 套在一份像订阅的配置上：节点加在列表末尾，同名的旧节点被替换；规则插在最前面，订阅自己
// 的规则一条不少地跟在后面。
func TestOverlayScriptRuns(t *testing.T) {
	if _, err := exec.LookPath("osascript"); err != nil {
		t.Skip("没有 osascript")
	}
	files, err := overlayFiles(overlayTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	var script string
	for _, f := range files {
		if f.name == "overlay.js" {
			script = f.content
		}
	}
	subscription := `{"proxies":[{"name":"HK"},{"name":"sidedoor","port":1}],"rules":["GEOIP,CN,DIRECT","MATCH,HK"]}`
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e", script+"\nJSON.stringify(main("+subscription+"))").CombinedOutput()
	if err != nil {
		t.Fatalf("脚本执行失败: %v\n%s", err, out)
	}
	var got struct {
		Proxies []struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		} `json:"proxies"`
		Rules []string `json:"rules"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("脚本输出不是 JSON: %v\n%s", err, out)
	}
	if len(got.Proxies) != 2 || got.Proxies[0].Name != "HK" || got.Proxies[1].Name != "sidedoor" || got.Proxies[1].Port != 11082 {
		t.Errorf("节点应当是 [HK, sidedoor:11082]，旧的同名节点被替换，实际 %+v", got.Proxies)
	}
	want := append(overlayRules(overlayTestConfig()), "GEOIP,CN,DIRECT", "MATCH,HK")
	if strings.Join(got.Rules, "\n") != strings.Join(want, "\n") {
		t.Errorf("规则应当是叠加层的在前、订阅的在后：\n实际 %q\n想要 %q", got.Rules, want)
	}
}
