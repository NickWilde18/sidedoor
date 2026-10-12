package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/mattn/go-runewidth"
)

// version 是发布时写进二进制的版本号：make VERSION=v0.1.0，见 Makefile 和发布用的 workflow。
// 自己编的是空的，sidedoor version 改为显示编译信息里的提交号。
var version = ""

func cmdVersion() error {
	info, _ := debug.ReadBuildInfo()
	fmt.Print(versionText(version, info))
	return nil
}

// versionText 是 sidedoor version 的输出：版本号或提交号、用的 sing-box、Go 的版本和平台。
func versionText(version string, info *debug.BuildInfo) string {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "  %s %s\n", runewidth.FillRight(label, 10), value)
	}
	if version != "" {
		fmt.Fprintf(&b, "sidedoor %s\n", version)
	} else {
		b.WriteString("sidedoor 开发版（自己编译的，不是发布版）\n")
	}
	if info == nil {
		return b.String()
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" {
		if len(revision) > 7 {
			revision = revision[:7]
		}
		if modified {
			revision += "，含未提交的改动"
		}
		line("提交", revision)
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/sagernet/sing-box" {
			singbox := dep.Version
			if dep.Replace != nil {
				singbox += "，打了 patches/ 里的补丁"
			}
			line("sing-box", singbox)
		}
	}
	line("Go", fmt.Sprintf("%s %s/%s", info.GoVersion, runtime.GOOS, runtime.GOARCH))
	return b.String()
}
