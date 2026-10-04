package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

// 命令行输出的几个小工具：带编号的步骤、对勾和提醒，让人看得出现在做到哪一步、
// 每一步具体做了什么、改了什么。

// 颜色只在输出到终端时加；输出到管道、日志文件，或者设了 NO_COLOR、TERM=dumb 时一律
// 不加。颜色只是辅助：✓、! 这些符号照旧都在，没有颜色也看得懂。
var (
	colorOut = colorFor(os.Stdout)
	colorErr = colorFor(os.Stderr)
)

func colorFor(f *os.File) bool {
	return wantColor(term.IsTerminal(int(f.Fd())), os.Getenv("NO_COLOR"), os.Getenv("TERM"))
}

// wantColor 是要不要上色的规则。按 NO_COLOR 的约定，它非空就不上色。
func wantColor(terminal bool, noColor, termName string) bool {
	return terminal && noColor == "" && termName != "dumb"
}

const (
	ansiBold   = "1"
	ansiDim    = "2"
	ansiRed    = "31"
	ansiGreen  = "32"
	ansiYellow = "33"
)

// paint 在 on 时给 s 套上 ANSI 颜色。
func paint(on bool, code, s string) string {
	if !on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

type steps struct{ n, total int }

func (s *steps) next(title string) {
	s.n++
	fmt.Printf("\n%s\n", paint(colorOut, ansiBold, fmt.Sprintf("[%d/%d] %s", s.n, s.total, title)))
}

// done 报告一件已经做完的事，对勾标绿。
func done(format string, args ...any) {
	fmt.Printf("  %s %s\n", paint(colorOut, ansiGreen, "✓"), fmt.Sprintf(format, args...))
}

// warn 报告一件需要人留意的事，整行标黄。
func warn(format string, args ...any) {
	fmt.Printf("  %s\n", paint(colorOut, ansiYellow, "! "+fmt.Sprintf(format, args...)))
}

// note 补充说明，不是结论。
func note(format string, args ...any) { fmt.Printf("    "+format+"\n", args...) }

// hint 是操作上的提示（怎么输入、还能试几次），标灰，和正文分开。
func hint(format string, args ...any) {
	fmt.Printf("    %s\n", paint(colorOut, ansiDim, fmt.Sprintf(format, args...)))
}

// row 打印状态里的一行。标签按显示宽度补齐，中英文混排也能对齐；以「! 」开头的值
// 是要留意的，标黄。
func row(label, value string) {
	if strings.HasPrefix(value, "! ") {
		value = paint(colorOut, ansiYellow, value)
	}
	fmt.Printf("  %s %s\n", runewidth.FillRight(label, 10), value)
}

func bytesText(n int64) string {
	if n < 0 {
		n = 0
	}
	return humanize.IBytes(uint64(n))
}

func rateText(n int64) string { return bytesText(n) + "/s" }

func durationText(d time.Duration) string {
	switch d = d.Round(time.Minute); {
	case d < time.Minute:
		return "1 分钟以内"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时 %d 分钟", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d 天 %d 小时", int(d.Hours())/24, int(d.Hours())%24)
	}
}
