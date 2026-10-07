package main

import "golang.org/x/sys/unix"

// 读写终端设置用的 ioctl，macOS 和 Linux 的名字不一样。
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
