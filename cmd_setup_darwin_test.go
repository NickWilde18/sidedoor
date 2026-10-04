//go:build darwin

package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
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

// TestSecretPromptAfterEchoOff 钉住「先打印 Password: 再关回显」：ReadPassword 被调用时才关
// 回显，提示在那之前就打出去了。看到提示就立刻输入的程序（T4 的 expect 脚本就是这样），
// 打的字会赶在回显关掉之前被回显出来，密码就进了终端记录。写出提示的那一刻回显必须已经
// 关了，读完之后再恢复。
func TestSecretPromptAfterEchoOff(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()
	if _, err := ptmx.Write([]byte("s3cret\n")); err != nil {
		t.Fatal(err)
	}

	var echoAtWrite []bool
	var prompt strings.Builder
	out := writerFunc(func(p []byte) (int, error) {
		echoAtWrite = append(echoAtWrite, echoOn(t, tty))
		prompt.Write(p)
		return len(p), nil
	})
	got, err := readSecret(tty, out, bufio.NewReader(tty), "Password")
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret" {
		t.Errorf("读到的密码是 %q，想要 %q", got, "s3cret")
	}
	if len(echoAtWrite) == 0 || echoAtWrite[0] {
		t.Errorf("写出密码提示的那一刻终端回显还开着（每次写出时的回显状态 %v）：看到提示就立刻输入的程序，打的字会被回显出来", echoAtWrite)
	}
	if !echoOn(t, tty) {
		t.Error("读完密码之后终端回显应当恢复")
	}
	// 输入时什么都不显示，提示里要说一声，不然容易以为卡住了。
	if !strings.Contains(prompt.String(), "输入时不显示") {
		t.Errorf("密码提示应当说明输入时不显示，实际 %q", prompt.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func echoOn(t *testing.T, tty *os.File) bool {
	t.Helper()
	termios, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	return termios.Lflag&unix.ECHO != 0
}
