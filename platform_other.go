//go:build !darwin

package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/kardianos/service"
)

// 其他系统（比如 Windows）还不支持，只是让代码编得过：setup 在选模式时就会停下。
// 计划见 docs/跨平台设计.md。

const serviceName = "sidedoor"

var systemConfigDir = filepath.Join(os.TempDir(), "sidedoor")

func userConfigDir() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "sidedoor")
}

func logDir(bool) string { return userConfigDir() }

func serviceFile(*Config) string { return "" }

func serviceOptions(*Config) service.KeyValue { return service.KeyValue{} }

func serviceNote(*Config) {}

func useKeychain(*Config) bool { return true }

func modeUnsupported(string) string { return "这个系统还不支持，目前只有 macOS 和 Linux" }

func systemProxySettings() (map[string]string, bool) { return nil, false }

func openBrowser(string) error { return errors.New("不支持") }
