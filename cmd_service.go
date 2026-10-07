package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"
	"github.com/sagernet/sing-box/adapter"
)

// newService 描述后台服务。服务怎么装、装在哪，见各平台的 serviceOptions。
func newService(cfg *Config, cfgPath string) (service.Service, error) {
	return service.New(noop{}, &service.Config{
		Name:        serviceName,
		DisplayName: "sidedoor",
		Description: "只让指定域名走 VPN，其余流量照常直连",
		Arguments:   []string{"run", "-config", cfgPath},
		Option:      serviceOptions(cfg),
	})
}

// noop 满足 service.Interface。launchd 和 systemd 直接执行 sidedoor run，用不到它的回调。
type noop struct{}

func (noop) Start(service.Service) error { return nil }
func (noop) Stop(service.Service) error  { return nil }

// logFile 是后台服务的日志。sing-box 和 sidedoor 自己都写 stderr，所以都在 .err.log 里。
func logFile(cfg *Config) string {
	return filepath.Join(logDir(cfg.Mode == ModeTUN), serviceName+".err.log")
}

// installService 装好并启动后台服务。重复 setup 时先卸掉旧的再装，保证幂等。
func installService(cfg *Config, cfgPath string) error {
	// 后台服务里记的是当前可执行文件的路径。go run 的临时文件过后就被删了，
	// 装上的任务会起不来。
	if exe, err := os.Executable(); err == nil && strings.Contains(exe, "go-build") {
		return errors.New("当前是 go run 的临时二进制，先 make install，再用装好的 sidedoor setup")
	}
	s, err := newService(cfg, cfgPath)
	if err != nil {
		return err
	}
	// launchd 和 systemd 都不会替我们建日志目录，目录不存在时服务直接起不来。
	if err := os.MkdirAll(logDir(cfg.Mode == ModeTUN), 0o755); err != nil {
		return err
	}
	_ = s.Stop()
	_ = s.Uninstall()
	if err := s.Install(); err != nil {
		return fmt.Errorf("安装后台服务: %w", err)
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("启动后台服务: %w", err)
	}
	return nil
}

// stopService 停掉已装的服务，返回停之前它是否在运行。
func stopService(cfg *Config, cfgPath string) bool {
	s, err := newService(cfg, cfgPath)
	if err != nil {
		return false
	}
	if st, err := s.Status(); err != nil || st != service.StatusRunning {
		return false
	}
	return s.Stop() == nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件路径，默认自动查找")
	verbose := fs.Bool("v", false, "输出每条连接的日志")
	_ = fs.Parse(args)

	path, cfg, err := configFromFlag(*cfgPath)
	if err != nil {
		return err
	}
	// 旧版本写的配置没有状态接口的地址和密钥，补上并存盘，status 才读得到。
	if cfg.fillDefaults() {
		if err := cfg.save(path); err != nil {
			return err
		}
	}
	password, err := cfg.password()
	if err != nil {
		return err
	}
	// 调试接口开不起来不影响隧道和分流，记一行日志接着跑。
	if closeDebug, err := serveDebug(debugSocketPath(path)); err != nil {
		log.Printf("调试接口没开起来: %v", err)
	} else {
		defer closeDebug()
	}
	// launchd 和 systemd 停止服务时发 SIGTERM，前台运行时是 Ctrl-C，都要走到 sing-box 的 Close，
	// 它会撤掉系统代理和路由。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runEngine(ctx, cfg, path, password, *verbose)
}

func configFromFlag(path string) (string, *Config, error) {
	if path == "" {
		return findConfig()
	}
	cfg, err := loadConfig(path)
	return path, cfg, err
}

func cmdStatus() error {
	path, cfg, err := findConfig()
	if err != nil {
		return err
	}
	// 普通用户的 launchctl list 看不到 root 的系统级任务，会把运行中误报成已停止。
	if err := requireRoot(cfg); err != nil {
		return err
	}
	fmt.Println("sidedoor 状态")
	account := cfg.Username
	if cfg.Group != "" {
		account += "，组 " + cfg.Group
	}
	row("服务器", fmt.Sprintf("%s（%s）", cfg.Server, account))
	row("走 VPN", strings.Join(cfg.Domains, ", "))
	row("模式", modeText(cfg))

	s, err := newService(cfg, path)
	if err != nil {
		return err
	}
	st, err := s.Status()
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		row("后台服务", "未安装，运行 sidedoor setup")
		return nil
	case err != nil:
		return err
	case st != service.StatusRunning:
		row("后台服务", "已停止，运行 sidedoor start")
		return nil
	}
	rs, err := readStatus(statusPath(path))
	if err != nil {
		row("后台服务", "运行中，还没写出状态，可能正在启动")
		return nil
	}
	row("后台服务", fmt.Sprintf("运行中（PID %d），已运行 %s", rs.PID, durationText(time.Since(rs.Started))))
	if rs.State == adapter.OpenConnectStateConnected {
		row("隧道", fmt.Sprintf("已连接 %s（%s 起），地址 %s", durationText(time.Since(rs.Since)),
			rs.Since.Local().Format("15:04"), strings.Join(rs.Address, ", ")))
		if len(rs.DNS) > 0 {
			row("DNS", strings.Join(rs.DNS, ", ")+"（学校域名经隧道问它）")
		}
	} else {
		row("隧道", rs.Message)
	}
	if cfg.Mode == ModeSystem {
		row("系统代理", systemProxyText(cfg.Listen))
	}

	t := rs.Traffic
	row("流量", "走 VPN  "+routeText(t.VPN))
	row("", "直连    "+routeText(t.Direct))
	row("", "（从后台服务启动时算起；速率是最近 "+statsInterval.String()+" 的平均）")
	printVPNConnections(cfg)

	if cfg.Dashboard {
		row("网页面板", "http://"+cfg.API+"/ui/")
	} else {
		row("网页面板", "未开启，运行 sidedoor dashboard")
	}
	if cfg.Overlay != "" {
		row("叠加层", cfg.Overlay+"（只转发走 VPN 的域名），文件在 "+overlayDir(path))
	} else {
		row("叠加层", "未开启；和别的代理软件一起用时运行 sidedoor overlay")
	}
	row("日志", logFile(cfg))
	return nil
}

func modeText(cfg *Config) string {
	switch cfg.Mode {
	case ModeProxy:
		return "proxy：只开本地代理 " + cfg.Listen
	case ModeSystem:
		return "system：本地代理 " + cfg.Listen + "，系统代理指向它"
	case ModeTUN:
		return "tun：虚拟网卡，外加本地代理 " + cfg.Listen
	}
	return cfg.Mode
}

func systemProxyText(listen string) string {
	if systemProxyIs(listen) {
		return "已指向 " + listen
	}
	if owner := systemProxyOwner(listen); owner != "" {
		return "! 现在指向 " + owner + "，不是 sidedoor（可能被别的工具改了），运行 sidedoor restart 改回来"
	}
	return "! 没有开启，运行 sidedoor restart"
}

func routeText(r routeStats) string {
	return fmt.Sprintf("↑ %-9s ↓ %-9s 当前 %d 个连接，速率 ↑ %s ↓ %s",
		bytesText(r.Upload), bytesText(r.Download), r.Active, rateText(r.UploadRate), rateText(r.DownloadRate))
}

// apiConnection 是状态接口 /connections 返回的一条连接，只取用得上的字段。
type apiConnection struct {
	Metadata struct {
		Host            string `json:"host"`
		DestinationIP   string `json:"destinationIP"`
		DestinationPort string `json:"destinationPort"`
	} `json:"metadata"`
	Upload   int64     `json:"upload"`
	Download int64     `json:"download"`
	Start    time.Time `json:"start"`
	Chains   []string  `json:"chains"`
}

func (c apiConnection) target() string {
	host := c.Metadata.Host
	if host == "" {
		host = c.Metadata.DestinationIP
	}
	return net.JoinHostPort(host, c.Metadata.DestinationPort)
}

// apiClient 访问本机的状态接口。显式不走任何代理，免得请求绕到 sidedoor 自己身上。
var apiClient = &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}

// fetchConnections 从状态接口取当前所有连接。
func fetchConnections(cfg *Config) ([]apiConnection, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+cfg.API+"/connections", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APISecret)
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("状态接口返回 %s", resp.Status)
	}
	var body struct {
		Connections []apiConnection `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Connections, nil
}

// printVPNConnections 列出当前走 VPN 的连接，按传输量从大到小，最多 10 条。
func printVPNConnections(cfg *Config) {
	conns, err := fetchConnections(cfg)
	if err != nil {
		row("VPN 连接", "读不到："+err.Error())
		return
	}
	var vpn []apiConnection
	for _, c := range conns {
		if slices.Contains(c.Chains, "vpn") {
			vpn = append(vpn, c)
		}
	}
	if len(vpn) == 0 {
		row("VPN 连接", "当前没有")
		return
	}
	slices.SortFunc(vpn, func(a, b apiConnection) int {
		return int((b.Upload + b.Download) - (a.Upload + a.Download))
	})
	row("VPN 连接", fmt.Sprintf("当前 %d 条", len(vpn)))
	for i, c := range vpn[:min(len(vpn), 10)] {
		fmt.Printf("    %2d. %-40s ↓ %-9s ↑ %-9s 持续 %s\n", i+1, c.target(),
			bytesText(c.Download), bytesText(c.Upload), durationText(time.Since(c.Start)))
	}
	if len(vpn) > 10 {
		note("还有 %d 条，完整列表看 sidedoor dashboard", len(vpn)-10)
	}
}

func cmdControl(action string) error {
	path, cfg, err := findConfig()
	if err != nil {
		return err
	}
	if err := requireRoot(cfg); err != nil {
		return err
	}
	s, err := newService(cfg, path)
	if err != nil {
		return err
	}
	since := time.Now()
	if err := service.Control(s, action); err != nil {
		return err
	}
	if action == "stop" {
		done("后台服务已停止")
		if cfg.Mode == ModeSystem {
			reportProxyRestored(cfg.Listen)
		}
		return nil
	}
	done("后台服务已%s，等它连上隧道……", map[string]string{"start": "启动", "restart": "重启"}[action])
	reportTunnel(path, cfg, since)
	return nil
}

// reportProxyRestored 确认停止服务后系统代理已经撤掉。sing-box 退出时才撤，所以等一会儿。
func reportProxyRestored(listen string) {
	for range 10 {
		if !systemProxyIs(listen) {
			done("系统代理已还原")
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	warn("系统代理还指向 %s：可以在「系统设置 → 网络 → 详细信息 → 代理」里手动关掉", listen)
}

// cmdUninstall 卸载服务，删除保存的密码和配置目录里 sidedoor 自己的文件。
// 日志留着，出了问题还能翻。
func cmdUninstall() error {
	path, cfg, err := findConfig()
	if err != nil {
		return err
	}
	if err := requireRoot(cfg); err != nil {
		return err
	}
	s, err := newService(cfg, path)
	if err != nil {
		return err
	}
	_ = s.Stop()
	if err := s.Uninstall(); err != nil {
		return fmt.Errorf("卸载后台服务: %w", err)
	}
	done("后台服务已停止并卸载（删除了 %s）", serviceFile(cfg))
	if cfg.Mode == ModeSystem {
		reportProxyRestored(cfg.Listen)
	}
	if cfg.Mode == ModeTUN {
		tunDown(cfg)
		done("%s", tunCleanupText())
	}
	if err := cfg.deletePassword(); err != nil {
		return fmt.Errorf("删除保存的密码: %w", err)
	}
	done("已删除保存的密码（%s）", passwordPlace(cfg))
	dir := filepath.Dir(path)
	for _, name := range []string{"config.json", "status.json", "cache.db", "debug.sock"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	_ = os.RemoveAll(filepath.Join(dir, "ui"))
	_ = os.RemoveAll(overlayDir(path))
	_ = os.Remove(dir) // 目录里还有别的东西就留着
	done("已删除配置 %s", dir)
	note("日志留在 %s，不需要的话可以手动删掉", logDir(cfg.Mode == ModeTUN))
	return nil
}

// cmdDashboard 打开网页面板。
//
// 面板文件第一次用时由 sing-box 从 GitHub 下载，所以默认不开：平时启动后台服务
// 不依赖 GitHub，只有第一次运行这个命令时才去下载。
func cmdDashboard() error {
	path, cfg, err := findConfig()
	if err != nil {
		return err
	}
	if err := requireRoot(cfg); err != nil {
		return err
	}
	s := &steps{total: 3}
	s.next("打开网页面板")
	if cfg.Dashboard {
		done("之前已经打开过")
	} else {
		cfg.Dashboard = true
		if err := cfg.save(path); err != nil {
			return err
		}
		done("已在配置里打开")
		svc, err := newService(cfg, path)
		if err != nil {
			return err
		}
		note("重启后台服务，第一次会从 GitHub 下载面板文件，可能要等几十秒……")
		if err := service.Control(svc, "restart"); err != nil {
			return err
		}
		done("后台服务已重启")
	}

	s.next("等面板就绪")
	ui := "http://" + cfg.API + "/ui/"
	if !waitHTTP(ui, 90*time.Second) {
		return fmt.Errorf("90 秒内面板还没就绪，看看日志 %s", logFile(cfg))
	}
	done("面板已就绪：%s", ui)

	s.next("在浏览器里打开")
	// tun 模式是 sudo 运行的，不替 root 开浏览器，只把地址给出来。
	host, port, _ := net.SplitHostPort(cfg.API)
	if os.Geteuid() != 0 && openBrowser(dashboardURL(cfg)) == nil {
		done("已在浏览器里打开，面板会自动连上 sidedoor")
	} else {
		note("请在浏览器里打开 %s", dashboardURL(cfg))
		// 状态接口只监听这台机器的 127.0.0.1，经 SSH 登录的话，得先把端口转到自己电脑上。
		if os.Getenv("SSH_CONNECTION") != "" {
			note("你是经 SSH 登录的：先在自己电脑上运行 ssh -N -L %s:%s <这台机器>，再在自己电脑的浏览器里打开上面的地址", port, cfg.API)
		}
	}
	note("如果面板还停在「Connect to a backend」，手动填：")
	note("  Host %s，Port %s，Secret %s", host, port, cfg.APISecret)
	note("列表里的 127.0.0.1:9090 是面板自带的默认值，连不上，可以删掉。")
	return nil
}

// dashboardURL 是在浏览器里打开面板用的地址。
//
// 面板自带的默认后台是 127.0.0.1:9090，不告诉它就去连那里。它会读网址里的 hostname、
// port、secret，把对应的后台加上并选中，随后把这几个参数从地址栏里抹掉。
func dashboardURL(cfg *Config) string {
	host, port, _ := net.SplitHostPort(cfg.API)
	query := url.Values{"hostname": {host}, "port": {port}, "secret": {cfg.APISecret}}
	return "http://" + cfg.API + "/ui/?" + query.Encode()
}

// waitHTTP 等到 url 返回 200，最多等 timeout。
func waitHTTP(url string, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		if resp, err := apiClient.Get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}

// requireRoot 拦住用普通身份操作 tun 模式服务的情况：系统级任务只有 root 能动。
func requireRoot(cfg *Config) error {
	if cfg.Mode == ModeTUN && os.Geteuid() != 0 {
		return errNeedSudo()
	}
	return nil
}

// errNeedSudo 提示加 sudo，并给出要重新运行的完整命令。
func errNeedSudo() error {
	return fmt.Errorf("tun 模式的服务是用 sudo 装的，这个操作也要加 sudo：sudo sidedoor %s", strings.Join(os.Args[1:], " "))
}
