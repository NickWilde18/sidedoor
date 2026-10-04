package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/certificate"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport/fakeip"
	"github.com/sagernet/sing-box/dns/transport/local"
	_ "github.com/sagernet/sing-box/experimental/clashapi" // 在 init 里把状态接口注册进 sing-box
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/mixed"
	"github.com/sagernet/sing-box/protocol/openconnect"
	"github.com/sagernet/sing-box/protocol/tun"
	singjson "github.com/sagernet/sing/common/json"
)

// tun 模式用到的固定地址，都取自 RFC 5737 保留给文档示例的网段：现实网络里不会
// 出现，也不和常见 TUN 工具重叠（代理工具的假 IP 多用 198.18.0.0/15，组网工具多用
// 100.64.0.0/10）。要走 VPN 的域名在 tun 模式下被解析成 fakeIPRange 里的地址，
// 只有这一段被路由进我们的网卡，默认路由和其余路由一概不动。
const (
	fakeIPRange = "198.51.100.0/24"
	fakeIPProbe = "198.51.100.1"
	tunAddress  = "192.0.2.1/30"
	tunDNS      = "192.0.2.2"
)

// endpointOptions 是 VPN 端点的配置。
func endpointOptions(cfg *Config, password string) map[string]any {
	return map[string]any{
		"type": "openconnect",
		"tag":  "vpn",
		// 用户态协议栈：隧道里的 IP 包只在进程内流转，不建网卡、不改路由表和系统
		// DNS，所以不会和机器上已有的 VPN、代理工具冲突。
		"system":     false,
		"server":     cfg.Server,
		"username":   cfg.Username,
		"password":   password,
		"auth_group": cfg.Group,
		// 服务器自己的域名用本机 DNS 解析，不然会想经隧道去查，而隧道还没建起来。
		"domain_resolver": "local",
		// 默认重连 300 秒失败就彻底放弃。后台常驻要扛住合盖一整夜，所以放宽到
		// 一直重试；退避上限是 60 秒，不会频繁打扰服务器。
		"reconnect_timeout": "87600h",
	}
}

// buildOptions 把配置翻译成 sing-box 配置。结果只在内存里，不落盘——里面有密码。
func buildOptions(cfg *Config, cfgPath, password string, verbose bool) (map[string]any, error) {
	host, portText, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("监听地址 %q 不对: %w", cfg.Listen, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("监听端口 %q 不对", portText)
	}
	serverHost := cfg.serverHost()
	level := "warn"
	if verbose {
		level = "info"
	}

	options := map[string]any{
		"log": map[string]any{"level": level, "timestamp": true},
		"dns": map[string]any{
			"servers": []any{
				map[string]any{"type": "local", "tag": "local"},
				// 系统解析器：直连流量用它，和其他应用的解析结果一致，见 systemDNS。
				map[string]any{"type": systemDNSType, "tag": "system"},
				// 经隧道查询 VPN 服务器推送的 DNS，内网域名只有它认识。
				map[string]any{"type": "openconnect", "tag": "vpn-dns", "endpoint": "vpn", "accept_default_resolvers": true},
			},
			"final": "local",
		},
		"endpoints": []any{endpointOptions(cfg, password)},
		"inbounds": []any{map[string]any{
			"type":             "mixed",
			"tag":              "mixed-in",
			"listen":           host,
			"listen_port":      port,
			"set_system_proxy": cfg.Mode == ModeSystem,
		}},
		"outbounds": []any{map[string]any{"type": "direct", "tag": "direct"}},
		"route": map[string]any{
			"rules": []any{
				// 访问 VPN 服务器本身（比如它的网页门户）走直连，不绕进隧道。
				map[string]any{"domain": []string{serverHost}, "outbound": "direct"},
				// 要走 VPN 的域名：先经隧道 DNS 解析成内网地址，再交给 VPN 端点。
				map[string]any{"domain_suffix": cfg.Domains, "action": "resolve", "server": "vpn-dns", "strategy": "ipv4_only"},
				map[string]any{"domain_suffix": cfg.Domains, "outbound": "vpn"},
			},
			"final":                   "direct",
			"default_domain_resolver": "system",
		},
	}
	// 本地状态接口：sidedoor status 和网页面板从这里看当前每条连接走了哪、传了多少。
	// 只监听本机，并且要密钥，免得别的程序或者网页里的脚本来读。
	api := map[string]any{"external_controller": cfg.API, "secret": cfg.APISecret}
	if cfg.Dashboard {
		// 面板文件放在配置目录下。目录为空时，sing-box 启动时会去 GitHub 下载一次，
		// 所以默认不开，平时启动不依赖 GitHub，见 cmdDashboard。
		api["external_ui"] = filepath.Join(filepath.Dir(cfgPath), "ui")
	}
	options["experimental"] = map[string]any{"clash_api": api}
	if cfg.Overlay != "" {
		if err := addOverlay(options, cfg, serverHost); err != nil {
			return nil, err
		}
	}
	if cfg.Mode == ModeTUN {
		addTUN(options, cfg, serverHost)
	}
	return options, nil
}

// addOverlay 追加叠加层专用端口：代理软件里导入的叠加层把走 VPN 的域名转到这里。
//
// 这个端口只转发走 VPN 的域名，其余一律拒绝。代理软件的自动选择组拿它测速会失败，
// 不会把它当成普通节点选中；就算被手动选中，也只是普通网站打不开，不会悄悄把用户
// 自己的节点绕过去直连。
func addOverlay(options map[string]any, cfg *Config, serverHost string) error {
	host, portText, err := net.SplitHostPort(cfg.Overlay)
	if err != nil {
		return fmt.Errorf("叠加层端口 %q 不对: %w", cfg.Overlay, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("叠加层端口 %q 不对", portText)
	}
	options["inbounds"] = append(options["inbounds"].([]any), map[string]any{
		"type":        "mixed",
		"tag":         "overlay-in",
		"listen":      host,
		"listen_port": port,
	})
	route := options["route"].(map[string]any)
	route["rules"] = append([]any{
		// 代理软件只发来 IP 时，从 TLS 或 HTTP 里认出域名，后面的规则才认得。
		map[string]any{"inbound": "overlay-in", "action": "sniff"},
		// VPN 服务器本身不走 VPN；叠加层里已经让它直连，到这里说明规则顺序乱了。
		map[string]any{"inbound": "overlay-in", "domain": []string{serverHost}, "action": "reject"},
		map[string]any{
			"type": "logical", "mode": "and",
			"rules": []any{
				map[string]any{"inbound": "overlay-in"},
				map[string]any{"domain_suffix": cfg.Domains, "invert": true},
			},
			"action": "reject",
		},
	}, route["rules"].([]any)...)
	return nil
}

// addTUN 追加 tun 模式需要的部分：一块只接管假 IP 段的虚拟网卡，以及把要走 VPN
// 的域名解析成假 IP 的 DNS。系统只会把这些域名的查询发过来，见 tunUp。
func addTUN(options map[string]any, cfg *Config, serverHost string) {
	dnsOptions := options["dns"].(map[string]any)
	dnsOptions["servers"] = append(dnsOptions["servers"].([]any),
		map[string]any{"type": "fakeip", "tag": "fakeip", "inet4_range": fakeIPRange})
	dnsOptions["rules"] = []any{
		// VPN 服务器自己的名字也在这些后缀下面，要给真实地址：应用得能直接打开它的网页门户。
		map[string]any{"domain": []string{serverHost}, "server": "local"},
		// 只发 IPv4 假地址。AAAA 直接回空，免得应用拿到真实 IPv6 地址绕过隧道。
		map[string]any{"domain_suffix": cfg.Domains, "query_type": []string{"AAAA"}, "action": "predefined", "rcode": "NOERROR"},
		map[string]any{"domain_suffix": cfg.Domains, "server": "fakeip"},
	}
	options["inbounds"] = append(options["inbounds"].([]any), map[string]any{
		"type":       "tun",
		"tag":        "tun-in",
		"address":    []string{tunAddress},
		"auto_route": true,
		// 只接管假 IP 段，不碰默认路由。
		"route_address": []string{fakeIPRange},
	})
	route := options["route"].(map[string]any)
	route["rules"] = append([]any{
		map[string]any{"inbound": "tun-in", "port": 53, "action": "hijack-dns"},
		// 直连 VPN 服务器之前，先用 local 解析它的名字：系统解析器会按 /etc/resolver
		// 把这个名字交回给我们，拿到的是假 IP，连接就绕进自己的网卡里了。
		map[string]any{"domain": []string{serverHost}, "action": "resolve", "server": "local"},
	}, route["rules"].([]any)...)
	// 假 IP 和域名的对应关系存盘，服务重启后应用手里缓存的假 IP 依然有效。
	options["experimental"].(map[string]any)["cache_file"] = map[string]any{
		"enabled":      true,
		"path":         filepath.Join(configDir(true), "cache.db"),
		"store_fakeip": true,
	}
}

// newBox 创建 sing-box 实例。只注册用得上的几种协议。
func newBox(ctx context.Context, options map[string]any) (*box.Box, error) {
	ctx, parsed, err := parseOptions(ctx, options)
	if err != nil {
		return nil, err
	}
	return box.New(box.Options{Context: ctx, Options: parsed})
}

// parseOptions 把生成的配置交给 sing-box 解析，返回带着协议注册表的 ctx。
func parseOptions(ctx context.Context, options map[string]any) (context.Context, option.Options, error) {
	inbounds := inbound.NewRegistry()
	mixed.RegisterInbound(inbounds)
	tun.RegisterInbound(inbounds)
	outbounds := outbound.NewRegistry()
	direct.RegisterOutbound(outbounds)
	endpoints := endpoint.NewRegistry()
	openconnect.RegisterEndpoint(endpoints)
	dnsTransports := dns.NewTransportRegistry()
	local.RegisterTransport(dnsTransports)
	fakeip.RegisterTransport(dnsTransports)
	registerSystemDNS(dnsTransports)
	openconnect.RegisterDNSTransport(dnsTransports)
	ctx = box.Context(ctx, inbounds, outbounds, endpoints, dnsTransports, service.NewRegistry(), certificate.NewRegistry())

	raw, err := json.Marshal(options)
	if err != nil {
		return nil, option.Options{}, err
	}
	parsed, err := singjson.UnmarshalExtendedContext[option.Options](ctx, raw)
	if err != nil {
		return nil, option.Options{}, fmt.Errorf("生成的 sing-box 配置有误: %w", err)
	}
	// sing-box 只在写日志文件时才关颜色。后台服务的日志写到 stderr、由 launchd 存进文件，
	// 颜色码就跟着进了日志；stderr 不是终端时关掉，前台在终端里跑时照旧有颜色。
	if parsed.Log != nil {
		parsed.Log.DisableColor = !colorErr
	}
	return ctx, parsed, nil
}

func vpnEndpoint(instance *box.Box) (adapter.OpenConnectEndpoint, error) {
	raw, ok := instance.Endpoint().Get("vpn")
	if !ok {
		return nil, errors.New("找不到 vpn 端点")
	}
	ep, ok := raw.(adapter.OpenConnectEndpoint)
	if !ok {
		return nil, errors.New("vpn 端点不是 OpenConnect 端点")
	}
	return ep, nil
}

// runtimeStatus 是后台服务写给 sidedoor status 看的状态。隧道状态一变就覆盖，
// 另外每隔 statsInterval 刷新一次流量。
type runtimeStatus struct {
	PID     int          `json:"pid"`
	Started time.Time    `json:"started"`
	State   string       `json:"state"`
	Message string       `json:"message"`
	Address []string     `json:"address,omitempty"`
	DNS     []string     `json:"dns,omitempty"`
	Since   time.Time    `json:"since,omitzero"`
	Traffic trafficStats `json:"traffic"`
	Updated time.Time    `json:"updated"`
}

// statsInterval 是流量的刷新间隔，速率也按这个间隔算。测试里会调短。
var statsInterval = 5 * time.Second

// 日志上限。system 模式下所有应用都经过 sidedoor，别的应用直连失败也会被 sing-box
// 记进来（比如睡眠唤醒后系统 DNS 还没恢复的那几分钟），不设上限一年能涨到上 GB。
var (
	logLimit int64 = 10 << 20
	logKeep  int64 = 1 << 20
)

// trimLog 在日志超过 logLimit 时只保留最后 logKeep 字节，从一整行开始。
// launchd 以追加方式打开日志，截短之后新内容会接着写在末尾。
func trimLog(path string) error {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= logLimit {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tail := data[int64(len(data))-min(logKeep, int64(len(data))):]
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	if err := os.Truncate(path, 0); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(tail)
	return err
}

func statusPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "status.json")
}

// terminalRetry 是隧道进入不可恢复的错误后，进程主动退出前等待的时间。退出后由
// launchd 重新拉起，相当于每隔这么久从头重试一次；间隔拉长，免得反复打扰服务器。
const terminalRetry = 5 * time.Minute

// runEngine 启动 sing-box 并一直跑到 ctx 取消。隧道状态每变一次就记一行日志、
// 覆盖一次状态文件。
func runEngine(ctx context.Context, cfg *Config, cfgPath, password string, verbose bool) error {
	options, err := buildOptions(cfg, cfgPath, password, verbose)
	if err != nil {
		return err
	}
	if cfg.Mode == ModeTUN {
		if err := tunUp(cfg); err != nil {
			return err
		}
		defer tunDown(cfg)
	}
	instance, err := newBox(ctx, options)
	if err != nil {
		return err
	}
	defer instance.Close()
	counted := &traffic{}
	instance.Router().AppendTracker(counted)
	if err := instance.Start(); err != nil {
		return err
	}
	ep, err := vpnEndpoint(instance)
	if err != nil {
		return err
	}

	path := statusPath(cfgPath)
	defer os.Remove(path)
	log.Printf("已启动：%s 模式，本地代理 %s，状态接口 %s", cfg.Mode, cfg.Listen, cfg.API)
	started := time.Now()
	logPath, trimmedAt := logFile(cfg), started
	if err := trimLog(logPath); err != nil {
		log.Printf("截短日志失败: %v", err)
	}
	ticker := time.NewTicker(statsInterval)
	defer ticker.Stop()
	// rated 是上一次按间隔算出速率的快照；中途因为隧道状态变化而写文件时，沿用它的速率。
	rated, ratedAt := counted.snapshot(), started
	var last string
	for {
		// 先取通道再读状态：通道在下一次状态变化时关闭，这样读完之后发生的变化不会漏掉。
		updated := ep.StatusUpdated()
		st := ep.OpenConnectStatus()
		rs := describe(st)
		if rs.Message != last {
			log.Print(rs.Message)
			last = rs.Message
		}
		rs.Started = started
		rs.Traffic = counted.snapshot().ratesFrom(rated)
		if err := writeStatus(path, rs); err != nil {
			log.Printf("写状态文件失败: %v", err)
		}
		var retry <-chan time.Time
		if st.State == adapter.OpenConnectStateError {
			retry = time.After(terminalRetry)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-updated:
		case now := <-ticker.C:
			rated = counted.snapshot().withRates(rated, now.Sub(ratedAt))
			ratedAt = now
			if now.Sub(trimmedAt) >= 10*time.Minute {
				if err := trimLog(logPath); err != nil {
					log.Printf("截短日志失败: %v", err)
				}
				trimmedAt = now
			}
		case <-retry:
			return fmt.Errorf("隧道出错 %v 仍未恢复：%s", terminalRetry, st.Error)
		}
	}
}

// describe 把 sing-box 的隧道状态翻译成给人看的一句话。
func describe(st adapter.OpenConnectStatus) runtimeStatus {
	rs := runtimeStatus{PID: os.Getpid(), State: st.State, Updated: time.Now()}
	switch st.State {
	case adapter.OpenConnectStateConnected:
		if info := st.TunnelInfo; info != nil {
			for _, p := range info.IPv4 {
				rs.Address = append(rs.Address, p.Addr().String())
			}
			for _, a := range info.DNS {
				rs.DNS = append(rs.DNS, a.String())
			}
			rs.Since = info.ConnectedSince
		}
		rs.Message = fmt.Sprintf("隧道已连接，分到的地址 %v", rs.Address)
	case adapter.OpenConnectStateAuthPending:
		// 后台没人能回答表单：多半是保存的密码被拒了，或服务器要求额外验证。
		// 这时只等，不重试，免得拿旧密码反复登录把账号锁掉。
		rs.Message = "服务器要求重新登录，请运行 sidedoor setup"
		if ch := st.AuthChallenge; ch != nil && ch.Error != "" {
			rs.Message = fmt.Sprintf("服务器要求重新登录（%s），请运行 sidedoor setup", ch.Error)
		}
	case adapter.OpenConnectStateError:
		rs.Message = "隧道出错：" + st.Error
	default:
		rs.Message = "正在连接"
		if st.Error != "" {
			rs.Message = "正在重连，上次的错误：" + st.Error
		}
	}
	return rs
}

func writeStatus(path string, rs runtimeStatus) error {
	data, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readStatus(path string) (*runtimeStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rs runtimeStatus
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}
