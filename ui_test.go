package main

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
)

// captureStdout 收集 fn 往标准输出打的内容。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = old
	_ = w.Close()
	return <-done
}

func TestWantColor(t *testing.T) {
	cases := []struct {
		name              string
		terminal          bool
		noColor, termName string
		want              bool
	}{
		{"输出到终端", true, "", "xterm-256color", true},
		{"输出到管道或日志文件", false, "", "xterm-256color", false},
		{"设了 NO_COLOR", true, "1", "xterm-256color", false},
		{"TERM=dumb", true, "", "dumb", false},
	}
	for _, c := range cases {
		if got := wantColor(c.terminal, c.noColor, c.termName); got != c.want {
			t.Errorf("%s: wantColor = %v，想要 %v", c.name, got, c.want)
		}
	}
}

var ansiCode = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestColorKeepsText 确认颜色只是套在文字外面：上色时有转义码，去掉之后和不上色时一字不差；
// 不上色时一个转义码都没有，管道和日志里干干净净。
func TestColorKeepsText(t *testing.T) {
	defer func(c bool) { colorOut = c }(colorOut)
	emit := func() {
		s := &steps{total: 2}
		s.next("登录 VPN 服务器")
		done("登录成功")
		warn("服务器提示：%s", "Login failed.")
		note("说明")
		hint("方括号里是默认值")
		row("系统代理", "! 没有开启")
		row("隧道", "已连接")
	}
	colorOut = false
	plain := captureStdout(t, emit)
	if strings.Contains(plain, "\x1b") {
		t.Errorf("不上色时不该有转义码，实际 %q", plain)
	}
	colorOut = true
	colored := captureStdout(t, emit)
	if !strings.Contains(colored, "\x1b[32m✓") || !strings.Contains(colored, "\x1b[33m! 服务器提示") {
		t.Errorf("上色时对勾应当是绿的、提醒应当是黄的，实际 %q", colored)
	}
	if got := ansiCode.ReplaceAllString(colored, ""); got != plain {
		t.Errorf("去掉颜色之后应当和不上色时一样：\n上色后去色 %q\n不上色     %q", got, plain)
	}
}

// TestAskHintsDefaultsOnce 确认「方括号里是默认值」只在第一次遇到默认值时说一遍。
func TestAskHintsDefaultsOnce(t *testing.T) {
	defer func(h bool) { defaultHinted = h }(defaultHinted)
	defaultHinted = false
	in := bufio.NewReader(strings.NewReader("\n\n"))
	out := captureStdout(t, func() {
		_, _ = ask(in, "VPN 服务器地址", "vpn.example.edu")
		_, _ = ask(in, "走 VPN 的域名后缀", "example.edu")
	})
	if n := strings.Count(out, "方括号里是默认值"); n != 1 {
		t.Errorf("默认值的提示应当只出现一次，实际 %d 次：%q", n, out)
	}
}
