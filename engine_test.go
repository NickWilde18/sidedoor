package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	M "github.com/sagernet/sing/common/metadata"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// testConfig 指向一个连不上的服务器：只验证配置和分流，不碰真实的 VPN。
func testConfig(t *testing.T, mode string) *Config {
	t.Helper()
	cfg := &Config{
		Server: "127.0.0.1:1", Username: "u", Domains: []string{"example.edu"},
		Listen: freeAddr(t), API: freeAddr(t), Mode: mode,
	}
	cfg.fillDefaults()
	return cfg
}

// TestOptionsAccepted 确认三种模式、开不开网页面板，生成的配置 sing-box 都认。只创建
// 不启动：系统代理、虚拟网卡和面板下载都要到 Start 才会动，测试不会改到本机设置。
func TestOptionsAccepted(t *testing.T) {
	for _, mode := range []string{ModeProxy, ModeSystem, ModeTUN} {
		for _, dashboard := range []bool{false, true} {
			cfg := testConfig(t, mode)
			cfg.Dashboard = dashboard
			options, err := buildOptions(cfg, filepath.Join(t.TempDir(), "config.json"), "pw", false)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := newBox(context.Background(), options)
			if err != nil {
				t.Fatalf("%s 模式（面板 %v）: %v", mode, dashboard, err)
			}
			_ = instance.Close()
		}
	}
}

// TestRouting 真的跑起 proxy 模式的引擎，确认分流接对了、流量记对了。
//
// 把 localhost 放进走 VPN 的列表：它在本机能解析、直连能通，所以只有分流规则真的
// 把它交给了 VPN 端点（而 VPN 连不上），请求才会失败。规则没接上的话，它会被直连到
// 本地的测试服务器，拿到 "direct"。
func TestRouting(t *testing.T) {
	defer func(d time.Duration) { statsInterval = d }(statsInterval)
	statsInterval = 50 * time.Millisecond

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

	// 同一个端口既是 SOCKS5（代理工具用）也是 HTTP 代理（系统代理用），两条路都要通。
	for _, scheme := range []string{"socks5", "http"} {
		client := &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: scheme, Host: cfg.Listen})},
		}
		resp, err := client.Get(backend.URL)
		if err != nil {
			t.Errorf("%s: 不在列表里的目标应当直连成功: %v", scheme, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "direct" {
			t.Errorf("%s: 直连拿到 %q", scheme, body)
		}

		resp, err = client.Get("http://localhost:" + port + "/")
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) == "direct" {
				t.Errorf("%s: 列表里的域名应当交给 VPN 端点，VPN 没连上就该失败；实际被直连了", scheme)
			}
		}
	}

	// 流量要记在直连名下，VPN 那条路一个字节都不该有。
	var rs *runtimeStatus
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if s, err := readStatus(statusPath(cfgPath)); err == nil && s.Traffic.Direct.Download > 0 {
			rs = s
			break
		}
	}
	if rs == nil {
		t.Fatal("状态文件里一直没有直连流量")
	}
	if rs.State != adapter.OpenConnectStateConnecting {
		t.Errorf("连不上服务器时状态应当是正在连接，实际 %+v", rs)
	}
	if v := rs.Traffic.VPN; v.Upload != 0 || v.Download != 0 {
		t.Errorf("VPN 没连上，不该记到流量：%+v", v)
	}

	// 状态接口：带对密钥能取到连接列表，密钥不对要被拒。
	if _, err := fetchConnections(cfg); err != nil {
		t.Errorf("带正确密钥访问状态接口: %v", err)
	}
	wrong := *cfg
	wrong.APISecret = "wrong"
	if _, err := fetchConnections(&wrong); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("密钥不对时状态接口应当返回 401，实际 %v", err)
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

type stubOutbound struct{ outbound.Adapter }

func (*stubOutbound) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return nil, errors.New("stub")
}

func (*stubOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, errors.New("stub")
}

// TestTrafficCounter 站在 sing-box 的位置收发：从客户端读到的算上传，写回客户端的
// 算下载；连接关两次只减一次；没有出口的（比如本地 DNS 劫持）算直连。
func TestTrafficCounter(t *testing.T) {
	var tr traffic
	vpn := &stubOutbound{Adapter: outbound.NewAdapter("openconnect", "vpn", nil, nil)}
	inbound, app := net.Pipe()
	defer app.Close()
	conn := tr.RoutedConnection(context.Background(), inbound, adapter.InboundContext{}, nil, vpn)

	go func() { _, _ = app.Write([]byte("hello world")) }()
	if _, err := io.ReadFull(conn, make([]byte, 11)); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.ReadFull(app, make([]byte, 5)) }()
	if _, err := conn.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if got, want := tr.snapshot().VPN, (routeStats{Upload: 11, Download: 5, Active: 1}); got != want {
		t.Errorf("走 VPN 的计数 %+v，想要 %+v", got, want)
	}
	_ = conn.Close()
	_ = conn.Close()
	if got := tr.snapshot().VPN.Active; got != 0 {
		t.Errorf("关两次之后连接数应当是 0，实际 %d", got)
	}

	other, _ := net.Pipe()
	tr.RoutedConnection(context.Background(), other, adapter.InboundContext{}, nil, nil)
	if got := tr.snapshot(); got.Direct.Active != 1 || got.VPN.Active != 0 {
		t.Errorf("没有出口的连接应当算直连，实际 %+v", got)
	}
}

func TestWithRates(t *testing.T) {
	prev := trafficStats{VPN: routeStats{Upload: 100, Download: 1000}, Direct: routeStats{Download: 50}}
	cur := trafficStats{VPN: routeStats{Upload: 600, Download: 6000}, Direct: routeStats{Download: 10}}
	got := cur.withRates(prev, 5*time.Second)
	if got.VPN.UploadRate != 100 || got.VPN.DownloadRate != 1000 {
		t.Errorf("VPN 速率 ↑%d ↓%d，想要 ↑100 ↓1000", got.VPN.UploadRate, got.VPN.DownloadRate)
	}
	if got.Direct.DownloadRate != 0 {
		t.Errorf("计数变小（服务重启过）时速率应当记 0，实际 %d", got.Direct.DownloadRate)
	}
}

func TestTrimLog(t *testing.T) {
	defer func(limit, keep int64) { logLimit, logKeep = limit, keep }(logLimit, logKeep)
	logLimit, logKeep = 100, 30

	path := filepath.Join(t.TempDir(), "x.log")
	var all strings.Builder
	for i := range 10 {
		all.WriteString("line-0" + string(rune('0'+i)) + "\n")
	}
	if err := os.WriteFile(path, []byte(all.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := trimLog(path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != all.String() {
		t.Fatalf("没超上限时不该动日志，实际 %q", data)
	}

	for i := range 10 {
		all.WriteString("line-1" + string(rune('0'+i)) + "\n")
	}
	if err := os.WriteFile(path, []byte(all.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := trimLog(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if len(data) > 30 || !strings.HasPrefix(string(data), "line-") || !strings.HasSuffix(string(data), "line-19\n") {
		t.Fatalf("超上限后应当只留最后几行整行，实际 %q", data)
	}
}
