package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestVersionText(t *testing.T) {
	info := &debug.BuildInfo{
		GoVersion: "go1.27.1",
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "8892d66f0e1d2c3b4a5968778695a4b3c2d1e0f9"},
			{Key: "vcs.modified", Value: "true"},
		},
		Deps: []*debug.Module{{
			Path: "github.com/sagernet/sing-box", Version: "v1.14.2",
			Replace: &debug.Module{Path: "/work/sidedoor/.build/sing-box", Version: "(devel)"},
		}},
	}
	cases := []struct {
		name, version string
		info          *debug.BuildInfo
		want, not     []string
	}{
		{"发布版", "v0.1.0", info, []string{"sidedoor v0.1.0\n", "8892d66，含未提交的改动", "v1.14.2，打了 patches/ 里的补丁", "go1.27.1"}, []string{"开发版", "8892d66f"}},
		{"自己编的", "", info, []string{"开发版", "8892d66"}, nil},
		// 没有编译信息时（比如 go run 的某些情况）只说版本，不能出错。
		{"没有编译信息", "", nil, []string{"开发版"}, []string{"提交"}},
	}
	for _, c := range cases {
		got := versionText(c.version, c.info)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s：输出里应当有 %q，实际\n%s", c.name, w, got)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s：输出里不该有 %q，实际\n%s", c.name, n, got)
			}
		}
	}
}
