//go:build darwin

package main

import (
	"os"
	"testing"
)

func TestRouteConflict(t *testing.T) {
	// 真机抓的：别的 VPN 用 128.0.0.0/1 盖住了默认路由。它按最长前缀匹配会输给
	// 我们的 /24，不算冲突。
	split, err := os.ReadFile("testdata/route-get-split-default.txt")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, out string
		taken     bool
	}{
		{"被 /1 路由盖住的默认路由", string(split), false},
		{"默认路由", "   route to: 198.51.100.1\ndestination: default\n       mask: default\n    gateway: 192.168.1.1\n  interface: en0\n", false},
		{"更粗的 /16", "destination: 198.51.0.0\n       mask: 255.255.0.0\n  interface: utun3\n", false},
		{"一样的 /24", "destination: 198.51.100.0\n       mask: 255.255.255.0\n  interface: utun9\n", true},
		{"主机路由", "destination: 198.51.100.1\n  interface: utun9\n", true},
	}
	for _, c := range cases {
		if owner, taken := routeConflict([]byte(c.out)); taken != c.taken {
			t.Errorf("%s: routeConflict = %q, %v，想要 %v", c.name, owner, taken, c.taken)
		}
	}
}
