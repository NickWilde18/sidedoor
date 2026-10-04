//go:build !darwin

package main

// echoOff 在其他系统上什么也不做，回显照旧由 ReadPassword 关。
func echoOff(int) (restore func(), err error) { return func() {}, nil }
