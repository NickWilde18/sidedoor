//go:build darwin || linux

package main

import (
	"errors"
	"syscall"
)

// processAlive 判断进程还在不在：发 0 号信号只做检查，不真的发信号。进程属于别的用户
// （EPERM）也算在。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
