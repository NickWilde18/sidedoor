//go:build !darwin

package main

import "errors"

func tunUp(*Config) error { return errors.New("tun 模式目前只支持 macOS") }

func tunDown(*Config) {}
