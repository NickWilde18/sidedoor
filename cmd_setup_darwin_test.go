//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// TestInterruptRestoresTerminal 钉住「问密码时按 Ctrl-C，终端回显没恢复」：ReadPassword
// 先关掉回显、读完才恢复，而 Ctrl-C 让进程直接退出，恢复那一步就跳过了，之后在这个
// 终端里打字都看不见。在伪终端里起一个停在问密码处的子进程，给它发 SIGINT（按
// Ctrl-C 时进程收到的就是它），看它退出后回显有没有回来。
//
// 子进程不接管这个伪终端做控制终端：在 macOS 上，会话首进程一退出，内核就回收它的
// 控制终端，退出后就读不到回显状态了。
func TestInterruptRestoresTerminal(t *testing.T) {
	if os.Getenv("SIDEDOOR_PROMPT_HELPER") == "1" {
		defer guardInterrupt(func() {})()
		_, _ = askSecret(bufio.NewReader(os.Stdin), "Password")
		return
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInterruptRestoresTerminal$")
	cmd.Env = append(os.Environ(), "SIDEDOOR_PROMPT_HELPER=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = bytes.NewBuffer(nil).ReadFrom(ptmx) }() // 把子进程的输出读掉，免得写满阻塞

	// 等到回显真的被关掉（ReadPassword 已经在等输入）再发信号，免得抢在它前面。
	deadline := time.Now().Add(10 * time.Second)
	for echoOn(t, tty) {
		if time.Now().After(deadline) {
			t.Fatal("子进程一直没开始读密码")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("Ctrl-C 之后应当以 130 退出，实际 %v", err)
	}
	if !echoOn(t, tty) {
		t.Error("Ctrl-C 退出之后终端回显还关着")
	}
}

func echoOn(t *testing.T, tty *os.File) bool {
	t.Helper()
	termios, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	return termios.Lflag&unix.ECHO != 0
}
