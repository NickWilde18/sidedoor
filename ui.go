package main

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/mattn/go-runewidth"
)

// 命令行输出的几个小工具：带编号的步骤、对勾和提醒，让人看得出现在做到哪一步、
// 每一步具体做了什么、改了什么。

type steps struct{ n, total int }

func (s *steps) next(title string) {
	s.n++
	fmt.Printf("\n[%d/%d] %s\n", s.n, s.total, title)
}

// done 报告一件已经做完的事。
func done(format string, args ...any) { fmt.Printf("  ✓ "+format+"\n", args...) }

// warn 报告一件需要人留意的事。
func warn(format string, args ...any) { fmt.Printf("  ! "+format+"\n", args...) }

// note 补充说明，不是结论。
func note(format string, args ...any) { fmt.Printf("    "+format+"\n", args...) }

// row 打印状态里的一行。标签按显示宽度补齐，中英文混排也能对齐。
func row(label, value string) {
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
