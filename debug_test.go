package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDebugSocket 确认调试接口只有自己能连、能导出 goroutine 栈，关掉之后 socket 文件
// 也删了。路径放在短目录下：t.TempDir() 在 macOS 上太长，超过 Unix socket 的 104 字节上限。
func TestDebugSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := debugSocketPath(filepath.Join(dir, "config.json"))
	// 上次进程被 kill 掉时留下的旧文件，不该挡住这次启动。
	if err := os.WriteFile(socket, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	closeDebug, err := serveDebug(socket)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("调试 socket 应当是只有自己能读写的 socket，实际 %v", info.Mode())
	}

	body, err := fetchDebug(socket, "goroutine?debug=2")
	if err != nil {
		t.Fatal(err)
	}
	dump, _ := io.ReadAll(body)
	_ = body.Close()
	if !strings.Contains(string(dump), "goroutine ") || !strings.Contains(string(dump), "net/http.(*Server).Serve") {
		t.Errorf("应当导出所有 goroutine 的栈，实际开头是 %.200q", dump)
	}

	closeDebug()
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("关掉之后 socket 文件应当删掉，实际 %v", err)
	}
}
