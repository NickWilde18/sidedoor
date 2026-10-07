//go:build !darwin

package main

import "errors"

var errTunUnsupported = errors.New("tun 模式目前只支持 macOS 和 Linux")

func tunInbound() map[string]any { return nil }

func tunPreflight(*Config) error { return errTunUnsupported }

func tunUp(*Config) error { return errTunUnsupported }

func tunStarted(*Config) error { return errTunUnsupported }

func tunDown(*Config) {}

func tunCleanupText() string { return "" }
