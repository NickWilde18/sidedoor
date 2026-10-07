//go:build darwin

package main

import "golang.org/x/sys/unix"

// echoOff 关掉终端回显，返回恢复原状的函数。
func echoOff(fd int) (restore func(), err error) {
	termios, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	saved := *termios
	termios.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, termios); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlWriteTermios, &saved) }, nil
}
