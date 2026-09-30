package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"golang.org/x/term"
)

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	server := fs.String("server", "", "VPN 服务器地址")
	mode := fs.String("mode", "", "proxy、system 或 tun，留空自动选择")
	domains := fs.String("domains", "", "走 VPN 的域名后缀，逗号分隔；留空时根据服务器地址推一个默认值")
	listen := fs.String("listen", defaultListen, "本地代理的监听地址")
	_ = fs.Parse(args)

	root := os.Geteuid() == 0
	switch {
	case *mode != "" && *mode != ModeProxy && *mode != ModeSystem && *mode != ModeTUN:
		return fmt.Errorf("没有 %q 这种模式，可选 proxy、system、tun", *mode)
	case *mode == ModeTUN && runtime.GOOS != "darwin":
		return errors.New("tun 模式目前只支持 macOS")
	case *mode == ModeTUN && !root:
		return errors.New("tun 模式要建虚拟网卡，请用 sudo sidedoor setup -mode tun")
	case *mode != ModeTUN && root:
		return errors.New("只有 tun 模式需要 sudo，其他模式请直接运行 sidedoor setup")
	}
	path := filepath.Join(configDir(root), "config.json")
	prev, _ := loadConfig(path)
	if prev == nil {
		prev = &Config{}
	}

	// 重新配置时先停掉旧服务：腾出端口，也免得服务器上同时挂着两个会话互相踢。
	stopService(prev, path)
	if err := checkPortFree(*listen); err != nil {
		return err
	}

	in := bufio.NewReader(os.Stdin)
	if *server == "" {
		s, err := ask(in, "VPN 服务器地址", prev.Server)
		if err != nil {
			return err
		}
		*server = s
	}
	host, err := normalizeServer(*server)
	if err != nil {
		return err
	}
	cfg := &Config{Server: host, Listen: *listen, Mode: *mode}

	fmt.Printf("正在连接 %s……\n", host)
	creds, err := login(cfg, in)
	if err != nil {
		return err
	}
	cfg.Username, cfg.Group = creds.username, creds.group
	if cfg.Username == "" || creds.password == "" {
		return errors.New("没认出登录表单里的用户名和密码，后台服务没法自动登录")
	}

	if *domains == "" {
		def := strings.Join(defaultDomains(host), ",")
		if len(prev.Domains) > 0 && prev.Server == host {
			def = strings.Join(prev.Domains, ",")
		}
		if *domains, err = ask(in, "走 VPN 的域名后缀（逗号分隔）", def); err != nil {
			return err
		}
	}
	if cfg.Domains = parseDomains(*domains); len(cfg.Domains) == 0 {
		return errors.New("至少要有一个走 VPN 的域名后缀")
	}
	if cfg.Mode == "" {
		cfg.Mode = pickMode(cfg.Listen)
	}

	if err := cfg.save(path); err != nil {
		return err
	}
	if err := cfg.savePassword(creds.password); err != nil {
		return fmt.Errorf("保存密码失败: %w", err)
	}
	if err := installService(cfg, path); err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("已装好，%s 模式，开机登录后自动在后台运行。\n", cfg.Mode)
	fmt.Printf("  走 VPN   %s\n", strings.Join(cfg.Domains, ", "))
	fmt.Printf("  本地代理 %s（SOCKS5 / HTTP）\n", cfg.Listen)
	fmt.Printf("  配置     %s\n", path)
	fmt.Println("  查看状态 sidedoor status")
	return nil
}

// credentials 是从登录表单里认出来的用户名、密码和认证组。
type credentials struct {
	username, password, group string
}

// remember 按和 sing-openconnect 相同的规则认出用户名、密码和认证组：文本框是
// 用户名，名叫 password 的密码框是密码，名叫 group_list 的下拉框是认证组。后台
// 服务通过 username、password、auth_group 三个选项把它们自动填回同一个表单。
func (c *credentials) remember(field adapter.OpenConnectAuthFormField, value string) {
	switch {
	case field.Name == "group_list":
		c.group = value
	case field.Kind == "password" && field.Name == "password":
		c.password = value
	case field.Kind == "text" && (field.Name == "username" || c.username == ""):
		c.username = value
	}
}

// login 不带凭据启动一次 VPN 端点，把服务器发来的登录表单原样问用户，直到隧道连上。
//
// 这样不用猜服务器要填哪些字段：认证组下拉框、以后可能加的二次验证都走同一条路。
// 连上就说明凭据有效，随即断开；后台服务启动时再用保存的凭据重新登录。
func login(cfg *Config, in *bufio.Reader) (credentials, error) {
	var creds credentials
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	instance, err := newBox(ctx, map[string]any{
		"log":       map[string]any{"level": "error"},
		"dns":       map[string]any{"servers": []any{map[string]any{"type": "local", "tag": "local"}}},
		"endpoints": []any{endpointOptions(cfg, "")},
	})
	if err != nil {
		return creds, err
	}
	defer instance.Close()
	if err := instance.Start(); err != nil {
		return creds, err
	}
	ep, err := vpnEndpoint(instance)
	if err != nil {
		return creds, err
	}

	answered := map[string]bool{}
	for {
		updated := ep.StatusUpdated()
		st := ep.OpenConnectStatus()
		switch st.State {
		case adapter.OpenConnectStateConnected:
			if info := st.TunnelInfo; info != nil {
				fmt.Printf("登录成功，分到的地址 %v\n", info.IPv4)
			}
			return creds, nil
		case adapter.OpenConnectStateError:
			return creds, fmt.Errorf("登录失败：%s", st.Error)
		case adapter.OpenConnectStateAuthPending:
			ch := st.AuthChallenge
			if ch == nil || answered[ch.ID] {
				break
			}
			answered[ch.ID] = true
			values, err := askChallenge(ch, in, &creds)
			if err != nil {
				_ = ep.CancelAuthChallenge(ch.ID)
				return creds, err
			}
			response := adapter.OpenConnectAuthResponse{Form: &adapter.OpenConnectAuthFormResponse{Values: values}}
			if err := ep.CompleteAuthChallenge(ch.ID, response); err != nil {
				return creds, fmt.Errorf("提交登录表单失败: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return creds, errors.New("已取消")
		case <-updated:
		}
	}
}

// askChallenge 把一份登录表单逐项问用户，返回按提交键组织的答案。
func askChallenge(ch *adapter.OpenConnectAuthChallenge, in *bufio.Reader, creds *credentials) (map[string]string, error) {
	if ch.Browser != nil {
		return nil, errors.New("服务器要求在浏览器里登录（SSO），sidedoor 暂不支持")
	}
	if ch.Form == nil {
		return nil, errors.New("服务器发来了一份空的登录表单")
	}
	for _, text := range []string{ch.Banner, ch.Message} {
		if text = strings.TrimSpace(text); text != "" {
			fmt.Println(text)
		}
	}
	if ch.Error != "" {
		fmt.Println("服务器提示：" + ch.Error)
	}
	values := make(map[string]string, len(ch.Form.Fields))
	for _, field := range ch.Form.Fields {
		label := strings.TrimSuffix(strings.TrimSpace(field.Label), ":")
		if label == "" {
			label = field.Name
		}
		var value string
		var err error
		switch {
		case field.Name == "group_list" && creds.group != "":
			// 服务器先单独问一轮组，再发一份组、用户名、密码俱全的完整表单。
			// 前一轮选过的组直接沿用，不再问第二遍。
			value = creds.group
			fmt.Printf("%s: %s\n", label, value)
		case field.Kind == "password":
			value, err = askSecret(in, label)
		case field.Kind == "select":
			value, err = askChoice(in, label, field.Options, field.Value)
		default:
			value, err = ask(in, label, field.Value)
		}
		if err != nil {
			return nil, err
		}
		values[field.SubmissionKey] = value
		creds.remember(field, value)
	}
	return values, nil
}

// ask 提问并读一行；直接回车取默认值。
func ask(in *bufio.Reader, label, def string) (string, error) {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	if s := strings.TrimSpace(line); s != "" {
		return s, nil
	}
	return def, nil
}

// askSecret 读密码：在终端里不回显；从管道读入时按普通的一行读，方便脚本和测试。
func askSecret(in *bufio.Reader, label string) (string, error) {
	fmt.Printf("%s: ", label)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println()
		return string(b), err
	}
	line, err := in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// askChoice 列出下拉框的选项让用户选编号，直接回车取服务器预选的那一项。
func askChoice(in *bufio.Reader, label string, options []adapter.OpenConnectAuthFormChoice, def string) (string, error) {
	if len(options) == 0 {
		return def, nil
	}
	fmt.Println(label)
	defIndex := 0
	for i, o := range options {
		mark := " "
		if o.Value == def {
			mark, defIndex = "*", i
		}
		name := o.Label
		if name == "" {
			name = o.Value
		}
		fmt.Printf("  %s %d) %s\n", mark, i+1, name)
	}
	for {
		s, err := ask(in, "选择编号", strconv.Itoa(defIndex+1))
		if err != nil {
			return "", err
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return options[n-1].Value, nil
		}
		fmt.Printf("请输入 1 到 %d 之间的数字\n", len(options))
	}
}

// pickMode 在没指定模式时自动选：系统代理已被别的工具占用，就只开本地代理，否则
// 接管系统代理。两个工具抢同一份系统代理设置，谁最后写谁生效，只会来回打架。
func pickMode(listen string) string {
	if owner := systemProxyOwner(listen); owner != "" {
		fmt.Printf("系统代理已被别的工具占用（%s），不去覆盖它，改用 proxy 模式。\n", owner)
		fmt.Printf("请在那个工具里把要走 VPN 的域名转发到 socks5://%s。\n", listen)
		return ModeProxy
	}
	return ModeSystem
}

// systemProxyOwner 返回当前系统代理指向的地址；没开系统代理，或者指向的就是
// listen，返回空串。目前只认 macOS。
func systemProxyOwner(listen string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		return ""
	}
	return proxyOwner(parseScutil(out), listen)
}

// parseScutil 解析 scutil --proxy 的输出，只取顶层的「键 : 值」行。
func parseScutil(out []byte) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, " : "); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func proxyOwner(m map[string]string, listen string) string {
	if m["ProxyAutoConfigEnable"] == "1" {
		return m["ProxyAutoConfigURLString"]
	}
	for _, kind := range []string{"HTTP", "HTTPS", "SOCKS"} {
		if m[kind+"Enable"] != "1" {
			continue
		}
		if addr := net.JoinHostPort(m[kind+"Proxy"], m[kind+"Port"]); addr != listen {
			return addr
		}
	}
	return ""
}

// checkPortFree 确认监听地址空着。刚停掉的旧服务可能还没退干净，所以多等几秒。
func checkPortFree(listen string) error {
	var err error
	for range 10 {
		var ln net.Listener
		if ln, err = net.Listen("tcp", listen); err == nil {
			return ln.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s 已被占用（旧的 ocproxy 之类还开着？）: %w", listen, err)
}
