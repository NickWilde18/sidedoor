package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSystemdUnit 钉住自己写单元模板的三个原因：日志追加写（sidedoor 截短日志之后新内容
// 接在末尾）、挂了 10 秒就拉起来而且一直拉、用户服务挂在 default.target 上（用户级的
// systemd 里没有 multi-user.target，挂在那上面开机和登录时都不会启动）。
func TestSystemdUnit(t *testing.T) {
	for _, c := range []struct {
		userService bool
		want, not   []string
	}{
		{true, []string{"WantedBy=default.target"}, []string{"multi-user.target", "network-online.target"}},
		{false, []string{"WantedBy=multi-user.target", "After=network-online.target"}, []string{"default.target"}},
	} {
		unit := systemdUnit(c.userService)
		common := []string{
			"ExecStart={{Path | cmdEscape}}{{range Arguments}} {{. | cmd}}{{end}}",
			"Restart=always", "RestartSec=10", "StartLimitIntervalSec=0",
			"StandardOutput=append:{{LogDirectory}}/{{Name}}.out.log",
			"StandardError=append:{{LogDirectory}}/{{Name}}.err.log",
		}
		for _, w := range append(common, c.want...) {
			if !strings.Contains(unit, w) {
				t.Errorf("用户服务 %v：单元里缺了 %q：\n%s", c.userService, w, unit)
			}
		}
		for _, n := range c.not {
			if strings.Contains(unit, n) {
				t.Errorf("用户服务 %v：单元里不该有 %q：\n%s", c.userService, n, unit)
			}
		}
	}
}

// TestServiceLogPath 确认单元里写日志的位置，就是 status 告诉用户、sidedoor 截短日志时用的位置。
func TestServiceLogPath(t *testing.T) {
	for _, mode := range []string{ModeProxy, ModeTUN} {
		cfg := &Config{Mode: mode}
		dir, _ := serviceOptions(cfg)["LogDirectory"].(string)
		if want := filepath.Join(dir, serviceName+".err.log"); logFile(cfg) != want {
			t.Errorf("%s 模式：单元写到 %s，status 和截短日志用的是 %s", mode, want, logFile(cfg))
		}
	}
}
