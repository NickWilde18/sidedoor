package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testConfig 指向一个连不上的服务器：只验证配置和分流，不碰真实的 VPN。
func testConfig(t *testing.T, mode string) *Config {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	_ = ln.Close()
	return &Config{Server: "127.0.0.1:1", Username: "u", Domains: []string{"example.edu"}, Listen: listen, Mode: mode}
}

// TestOptionsAccepted 确认三种模式生成的配置 sing-box 都认。只创建不启动：系统代理
// 和虚拟网卡都要到 Start 才会动，测试不会改到本机设置。
func TestOptionsAccepted(t *testing.T) {
	for _, mode := range []string{ModeProxy, ModeSystem, ModeTUN} {
		t.Run(mode, func(t *testing.T) {
			options, err := buildOptions(testConfig(t, mode), "pw", false)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := newBox(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			_ = instance.Close()
		})
	}
}

// TestRouting 真的跑起 proxy 模式的引擎，确认分流接对了。
//
// 把 localhost 放进走 VPN 的列表：它在本机能解析、直连能通，所以只有分流规则真的
// 把它交给了 VPN 端点（而 VPN 连不上），请求才会失败。规则没接上的话，它会被直连到
// 本地的测试服务器，拿到 "direct"。
func TestRouting(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())

	cfg := testConfig(t, ModeProxy)
	cfg.Domains = []string{"localhost"}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runEngine(ctx, cfg, cfgPath, "pw", false) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("runEngine: %v", err)
		}
	}()
	waitListening(t, cfg.Listen)

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "socks5", Host: cfg.Listen})},
	}
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("不在列表里的目标应当直连成功: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "direct" {
		t.Fatalf("直连拿到 %q", body)
	}

	if resp, err := client.Get("http://localhost:" + port + "/"); err == nil {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("列表里的域名应当交给 VPN 端点，VPN 没连上就该失败；实际被直连了，拿到 %q", body)
	}

	rs, err := readStatus(statusPath(cfgPath))
	if err != nil {
		t.Fatalf("后台应当写出状态文件: %v", err)
	}
	if rs.State != "connecting" || !strings.Contains(rs.Message, "连接") {
		t.Errorf("连不上服务器时状态应当是正在连接，实际 %+v", rs)
	}
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("tcp", addr); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s 一直没有开始监听", addr)
}
