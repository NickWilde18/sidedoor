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
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mDNS "github.com/miekg/dns"
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
		if modeUnsupported(mode) != "" {
			continue
		}
		for _, extras := range []struct{ dashboard, overlay bool }{{false, false}, {true, false}, {false, true}} {
			cfg := testConfig(t, mode)
			cfg.Dashboard = extras.dashboard
			if extras.overlay {
				cfg.Overlay = freeAddr(t)
			}
			options, err := buildOptions(cfg, filepath.Join(t.TempDir(), "config.json"), "pw", false)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := newBox(context.Background(), options)
			if err != nil {
				t.Fatalf("%s 模式（面板 %v，叠加层 %v）: %v", mode, extras.dashboard, extras.overlay, err)
			}
			_ = instance.Close()
		}
	}
}

// TestOverlayRejectsOtherTraffic 确认叠加层专用端口只转发走 VPN 的域名：代理软件的自动选择组
// 拿它测速、或者它被误选成普通节点时，别的目标必须被拒绝，不能被直连出去、悄悄绕开用户自己
// 的节点。同一个目标经普通的本地代理照常直连，说明是端口的规则在起作用。
func TestOverlayRejectsOtherTraffic(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "direct")
	}))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())

	cfg := testConfig(t, ModeProxy)
	cfg.Domains = []string{"localhost"}
	cfg.Overlay = freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runEngine(ctx, cfg, filepath.Join(t.TempDir(), "config.json"), "pw", false) }()
	defer func() {
		cancel()
		<-done
	}()
	waitListening(t, cfg.Overlay)

	for _, scheme := range []string{"socks5", "http"} {
		client := &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: scheme, Host: cfg.Overlay})},
		}
		// 不在列表里的目标：拒绝。列表里的域名：交给 VPN 端点（连不上），也到不了测试服务器。
		for _, target := range []string{backend.URL, "http://localhost:" + port + "/"} {
			if resp, err := client.Get(target); err == nil {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if string(body) == "direct" {
					t.Errorf("%s: 经叠加层端口访问 %s 被直连了，应当拒绝或交给 VPN", scheme, target)
				}
			}
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("经叠加层端口的请求不该到达测试服务器，实际到了 %d 次", n)
	}

	direct := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: cfg.Listen})},
	}
	resp, err := direct.Get(backend.URL)
	if err != nil {
		t.Fatalf("同一个目标经普通的本地代理应当直连成功: %v", err)
	}
	_ = resp.Body.Close()
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

// startFakeDNS 起一个只认 name 一个名字的 DNS 服务器，返回它的地址。
func startFakeDNS(t *testing.T, name, ip string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &mDNS.Server{PacketConn: pc, Handler: mDNS.HandlerFunc(func(w mDNS.ResponseWriter, req *mDNS.Msg) {
		resp := new(mDNS.Msg)
		resp.SetReply(req)
		q := req.Question[0]
		switch {
		case !strings.EqualFold(q.Name, name):
			resp.Rcode = mDNS.RcodeNameError
		case q.Qtype == mDNS.TypeA:
			resp.Answer = append(resp.Answer, &mDNS.A{
				Hdr: mDNS.RR_Header{Name: q.Name, Rrtype: mDNS.TypeA, Class: mDNS.ClassINET, Ttl: 60},
				A:   net.ParseIP(ip),
			})
		}
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })
	return pc.LocalAddr().String()
}

// TestDirectUsesSystemResolver 钉住「经 sidedoor 直连时，只有按域名分流的解析器才认识
// 的名字解析不出来」：sing-box 自带的 local 在 macOS 上只问默认网卡的那组 DNS，不认
// Tailscale 的 MagicDNS、别的 VPN 的内网域名、/etc/resolver 里的配置，浏览器经代理访问
// emberbound.tail5cd230.ts.net 就回 502。直连流量必须交给系统解析器；测试里用一个
// 只认 only-os.test 的假解析器代替它。
func TestDirectUsesSystemResolver(t *testing.T) {
	fake := startFakeDNS(t, "only-os.test.", "127.0.0.1")
	defer func(r *net.Resolver) { systemResolver = r }(systemResolver)
	systemResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", fake)
	}}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())

	cfg := testConfig(t, ModeProxy)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runEngine(ctx, cfg, cfgPath, "pw", false) }()
	defer func() {
		cancel()
		<-done
	}()
	waitListening(t, cfg.Listen)

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: cfg.Listen})},
	}
	resp, err := client.Get("http://only-os.test:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "direct" {
		t.Fatalf("只有系统解析器认识的名字，经代理直连应当能打开；实际 %s %q", resp.Status, body)
	}
}

// TestServiceLogHasNoColor 钉住「后台服务的日志文件里全是颜色码」：sing-box 只在写日志文件时才
// 关颜色，而后台服务的日志是写到 stderr、由 launchd 存进文件的。stderr 不是终端时要关掉颜色，
// 在终端里前台运行时照旧有颜色。
func TestServiceLogHasNoColor(t *testing.T) {
	defer func(c bool) { colorErr = c }(colorErr)
	for _, terminal := range []bool{false, true} {
		colorErr = terminal
		_, parsed, err := parseOptions(context.Background(), map[string]any{"log": map[string]any{"level": "warn"}})
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Log.DisableColor == terminal {
			t.Errorf("stderr 是终端：%v，DisableColor 应当是 %v，实际 %v", terminal, !terminal, parsed.Log.DisableColor)
		}
	}
}

// TestStatusIgnoresDeadProcess 钉住「服务进程被 kill -9 之后，status 还报告已连接」：进程来不及
// 删状态文件，systemd（launchd 也一样）在重新拉起之前那几秒里报告服务还在启动，status 就照着
// 旧文件显示「运行中（PID 旧的）… 已连接」，而这时本地代理根本没人监听。写状态的进程不在了，
// 这份状态就不能算数。
func TestStatusIgnoresDeadProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")

	exited := exec.Command(os.Args[0], "-test.run=^$")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	if err := writeStatus(path, runtimeStatus{PID: exited.Process.Pid, State: adapter.OpenConnectStateConnected}); err != nil {
		t.Fatal(err)
	}
	if rs, err := currentStatus(path); err == nil {
		t.Errorf("进程 %d 已经退出，它留下的状态不该算数；实际读到 %+v", exited.Process.Pid, rs)
	}

	if err := writeStatus(path, runtimeStatus{PID: os.Getpid(), State: adapter.OpenConnectStateConnected}); err != nil {
		t.Fatal(err)
	}
	if _, err := currentStatus(path); err != nil {
		t.Errorf("进程还在时应当读到状态，实际 %v", err)
	}
}
