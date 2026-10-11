package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
	if err := upgradeTunConfig(path, cfg); err != nil {
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

// configFromFlag 读 -config 指定的配置；没指定时读当前身份这一边的。
func configFromFlag(path string) (string, *Config, error) {
	if path == "" {
		path = sideConfigPath(ownSide())
	}
	cfg, err := loadConfig(path)
	return path, cfg, err
}

// cmdStatus 显示装着的模式和它的后台服务。不需要 sudo：tun 模式的 config.json 和状态文件
// 所有人都能读，服务停没停按文件判断（见 serviceState）。用 sudo 运行时，普通模式那一边切回
// 那个用户去显示：它的任务文件和日志在那个用户的家目录里。
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	only := sideFlag(fs)
	_ = fs.Parse(args)
	here, err := parseSide(*only)
	if err != nil {
		return err
	}
	if here != "" {
		return printSideStatus(here)
	}
	installed := installedSides()
	if len(installed) == 0 {
		return errors.New("还没有配置，先运行 sidedoor setup")
	}
	if len(installed) > 1 {
		warn("%v", errBothInstalled)
	}
	for i, s := range installed {
		if i > 0 {
			fmt.Println()
		}
		if s == sideUser && ownSide() != sideUser {
			if err := runSwitched(s, "status", "-side", string(s)); err != nil {
				return err
			}
			continue
		}
		if err := printSideStatus(s); err != nil {
			return err
		}
	}
	return nil
}

// printSideStatus 显示 s 那一边的配置和后台服务。
func printSideStatus(s side) error {
	path := sideConfigPath(s)
	cfg, err := loadConfig(path)
	if errors.Is(err, fs.ErrPermission) && s == sideTUN {
		// 旧版本写的 tun 配置只有 root 能读，下次以 root 身份启动时会改成新格式。
		fmt.Println("sidedoor 状态")
		row("模式", "tun（配置是旧版本写的，只有 root 能读；运行一次 sidedoor restart 之后这里就能显示完整内容）")
		cfg = &Config{Mode: ModeTUN}
	} else if err != nil {
		return err
	} else {
		fmt.Println("sidedoor 状态")
		account := cfg.Username
		if cfg.Group != "" {
			account += "，组 " + cfg.Group
		}
		row("服务器", fmt.Sprintf("%s（%s）", cfg.Server, account))
		row("走 VPN", strings.Join(cfg.Domains, ", "))
		row("模式", modeText(cfg))
	}

	st, rs := serviceState(cfg, path)
	switch st {
	case stateStopped:
		row("后台服务", "已停止，没有在代理任何流量；恢复用 sidedoor start")
		return nil
	case stateStarting:
		row("后台服务", "! 正在启动，或者刚退出、正被重新拉起来，过几秒再看；一直这样就运行 sidedoor restart，还不行看日志 "+logFile(cfg))
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
	printVPNConnections(rs)

	if cfg.API == "" {
		return nil
	}
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

// printVPNConnections 列出后台服务写在状态文件里的走 VPN 的连接，已经按传输量从大到小排好。
func printVPNConnections(rs *runtimeStatus) {
	if rs.VPNCount == 0 {
		row("VPN 连接", "当前没有")
		return
	}
	row("VPN 连接", fmt.Sprintf("当前 %d 条", rs.VPNCount))
	for i, c := range rs.VPN {
		fmt.Printf("    %2d. %-40s ↓ %-9s ↑ %-9s 持续 %s\n", i+1, c.target(),
			bytesText(c.Download), bytesText(c.Upload), durationText(time.Since(c.Start)))
	}
	if rs.VPNCount > len(rs.VPN) {
		note("还有 %d 条，完整列表看 sidedoor dashboard", rs.VPNCount-len(rs.VPN))
	}
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

// serviceRemover 是卸载用到的两个服务方法，测试里换成假的。
type serviceRemover interface {
	Stop() error
	Uninstall() error
}

// errServiceGone 表示卸载时出了错，但服务文件已经不在了。
var errServiceGone = errors.New("服务文件已经不在了")

// uninstallService 停掉并卸掉后台服务，file 是它的服务文件。
//
// 服务文件已经不在了（上次卸载卸到一半，或者被手动删掉了）时，卸载多半会报错，比如
// systemctl disable 说单元不存在。这种错不该挡住后面删密码和配置，不然 sidedoor uninstall
// 每次都停在同一处，剩下的东西永远删不掉。服务文件还在时，失败就是失败。
func uninstallService(s serviceRemover, file string) error {
	_ = s.Stop()
	err := s.Uninstall()
	if err == nil {
		return nil
	}
	if _, statErr := os.Stat(file); errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("%w：%s（卸载时报 %v）", errServiceGone, file, err)
	}
	return err
}

// cmdDashboard 打开网页面板。
//
// 面板文件第一次用时由 sing-box 从 GitHub 下载，所以默认不开：平时启动后台服务
// 不依赖 GitHub，只有第一次运行这个命令时才去下载。
func cmdDashboard(args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	only := sideFlag(fs)
	_ = fs.Parse(args)
	if switched, err := switchIfNeeded(*only); switched || err != nil {
		return err
	}
	path, cfg, err := loadSide(ownSide())
	if err != nil {
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
