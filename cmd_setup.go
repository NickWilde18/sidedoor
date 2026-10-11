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
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"golang.org/x/term"
)

// cmdSetup 登录一次并装成后台服务。加不加 sudo 都能装三种模式中的任何一种：模式定下来之后，
// 按 planSetup 排出步骤，不归当前身份管的那一步切换身份去做（见 lifecycle.go）。同一时刻
// 只留一种模式：另一边装着旧模式时，先停掉它，新模式装好之后再卸掉；没装成就把它重新启动。
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	server := fs.String("server", "", "VPN 服务器地址")
	mode := fs.String("mode", "", "proxy、system 或 tun；留空时会列出来让你选")
	domains := fs.String("domains", "", "走 VPN 的域名后缀，逗号分隔；留空时根据服务器地址推一个默认值")
	listen := fs.String("listen", defaultListen, "本地代理的监听地址")
	only := sideFlag(fs)
	_ = fs.Parse(args)

	switch {
	case *mode != "" && *mode != ModeProxy && *mode != ModeSystem && *mode != ModeTUN:
		return fmt.Errorf("没有 %q 这种模式，可选 proxy、system、tun", *mode)
	case *mode != "" && modeUnsupported(*mode) != "":
		return errors.New(modeUnsupported(*mode))
	}
	here, err := parseSide(*only)
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	opts := setupOptions{server: *server, mode: *mode, domains: *domains, listen: *listen}
	if here != "" {
		// 切换身份之后的那个 sidedoor：模式由切换前的那个定好了，只在这一边装。
		if sideOf(opts.mode) != here {
			return fmt.Errorf("-side %s 装不了 %s 模式", here, opts.mode)
		}
		return setupHere(in, opts)
	}

	fmt.Println("sidedoor setup：登录一次 VPN，然后装成后台服务。")
	if opts.mode == "" {
		m, err := askMode(in, opts.listen, systemProxyOwner(opts.listen))
		if err != nil {
			return err
		}
		opts.mode = m
	}
	plan := planSetup(ownSide(), sideOf(opts.mode), installedSides())
	if !plan.Install.Local {
		// 先确认切换得过去，免得旧模式已经停了才发现装不了。
		if _, err := switchCommand(plan.Install.Side, "sidedoor", nil); err != nil {
			return err
		}
	}
	plan.Restore = onlyRunning(plan.Restore, sideRunning)
	if len(plan.Stop) > 0 {
		note("这台电脑上还装着另一种模式，同一时刻只能有一种：先停掉它，新模式装好之后再卸掉")
		if err := runSteps(plan.Stop); err != nil {
			return err
		}
	}
	if plan.Install.Local {
		err = setupHere(in, opts)
	} else {
		err = runSwitched(plan.Install.Side, opts.args(plan.Install.Side, fs)...)
	}
	if err != nil {
		if len(plan.Restore) > 0 {
			if rerr := runSteps(plan.Restore); rerr != nil {
				warn("原来的模式没能重新启动：%v。可以手动运行 sidedoor start", rerr)
			} else {
				done("新模式没装成，已把原来的模式重新启动")
			}
		}
		return err
	}
	if len(plan.Cleanup) > 0 {
		if err := runSteps(plan.Cleanup); err != nil {
			return fmt.Errorf("新模式已经装好，原来的模式也停了，但没能把它卸干净：%w。重新运行一次 sidedoor setup -mode %s 会接着清理", err, opts.mode)
		}
		done("原来的模式已卸掉，现在只装着 %s 模式", opts.mode)
	}
	return nil
}

// setupOptions 是 setup 的参数。mode 在调用 setupHere 之前已经定好。
type setupOptions struct {
	server, mode, domains, listen string
}

// args 拼出切换身份后运行的 setup 的参数：带上 -side 和定好的模式，用户显式给过的参数照传。
func (o setupOptions) args(target side, fs *flag.FlagSet) []string {
	args := []string{"setup", "-side", string(target), "-mode", o.mode}
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "mode" && f.Name != "side" {
			args = append(args, "-"+f.Name, f.Value.String())
		}
	})
	return args
}

// setupHere 在当前身份这一边装 o.mode 模式：停掉这一边的旧服务、登录、选域名、保存并启动。
// 失败或者被 Ctrl-C 打断时，把这一边原来的服务恢复回去。
func setupHere(in *bufio.Reader, o setupOptions) error {
	server, domains := &o.server, &o.domains
	root := currentEUID() == 0
	path := filepath.Join(configDir(root), "config.json")
	prev, _ := loadConfig(path)
	if prev == nil {
		prev = &Config{}
	}
	// 重新配置时沿用状态接口的地址和密钥，登录过网页面板的不用重新填。
	cfg := &Config{Listen: o.listen, Mode: o.mode, Dashboard: prev.Dashboard, APISecret: prev.APISecret, Overlay: prev.Overlay}
	if prev.Listen == o.listen {
		cfg.API = prev.API
	}
	cfg.fillDefaults()

	s := &steps{total: 5}
	var replaced atomic.Bool // 新服务装上之后，原来的服务就不用恢复了

	s.next("检查环境")
	stopped := stopService(prev, path)
	// 没走到装新服务那一步就失败或者被 Ctrl-C 打断，要把原来的服务恢复回去，
	// 不然学校的连接就一直断着。
	restore := func() {
		if !stopped || replaced.Load() {
			return
		}
		if old, err := newService(prev, path); err == nil && old.Start() == nil {
			done("setup 没有完成，已恢复原来的后台服务")
		}
	}
	defer restore()
	defer guardInterrupt(restore)()
	if stopped {
		done("停掉了正在运行的 sidedoor 后台服务：腾出端口，也免得服务器上同时挂着两个会话")
	}
	addrs := []string{cfg.Listen, cfg.API}
	if cfg.Overlay != "" {
		addrs = append(addrs, cfg.Overlay)
	}
	for _, addr := range addrs {
		if err := checkPortFree(addr); err != nil {
			return err
		}
	}
	if cfg.Overlay != "" {
		done("本地端口空闲：%s（代理）、%s（状态接口）、%s（叠加层）", cfg.Listen, cfg.API, cfg.Overlay)
	} else {
		done("本地端口空闲：%s（代理）、%s（状态接口）", cfg.Listen, cfg.API)
	}
	// tun 模式的系统条件在这里先查一遍，免得装好服务才在日志里报错。
	if cfg.Mode == ModeTUN {
		if err := tunPreflight(cfg); err != nil {
			return err
		}
		done("可以建虚拟网卡：假 IP 段 %s 没被占用，分流 DNS 能用", fakeIPRange)
	}

	s.next("选择模式")
	done("%s 模式", cfg.Mode)

	s.next("登录 VPN 服务器")
	if *server == "" {
		v, err := ask(in, "VPN 服务器地址", prev.Server)
		if err != nil {
			return err
		}
		*server = v
	}
	host, err := normalizeServer(*server)
	if err != nil {
		return err
	}
	cfg.Server = host
	note("正在连接 %s，下面要填的内容都来自服务器发来的登录表单", host)
	creds, info, err := login(cfg, in)
	if err != nil {
		return err
	}
	cfg.Username, cfg.Group = creds.username, creds.group
	if cfg.Username == "" || creds.password == "" {
		return errors.New("没认出登录表单里的用户名和密码，后台服务没法自动登录")
	}
	done("登录成功，分到的地址 %s", prefixesText(info))
	note("这次登录只用来验证账号，已经断开；后台服务会用保存的凭据重新登录。")

	s.next("选择走 VPN 的域名")
	note("只有这些域名（含子域名）走 VPN，其余流量一律直连。")
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
	done("走 VPN：%s", strings.Join(cfg.Domains, ", "))

	s.next("保存配置并启动后台服务")
	if err := cfg.save(path); err != nil {
		return err
	}
	done("配置已写入 %s", path)
	// 叠加层文件里写着域名和服务器，跟着配置一起更新；规则变了的话，代理软件里要重新导入。
	if cfg.Overlay != "" {
		if _, err := writeOverlayFiles(path, cfg); err != nil {
			return err
		}
		if slices.Equal(overlayRules(prev), overlayRules(cfg)) {
			done("叠加层文件已更新：%s", overlayDir(path))
		} else {
			warn("走 VPN 的域名或服务器变了，叠加层文件已更新（%s），要在代理软件里重新导入一次", overlayDir(path))
		}
	}
	if err := cfg.savePassword(creds.password); err != nil {
		return fmt.Errorf("保存密码失败: %w", err)
	}
	done("密码已存入%s", passwordPlace(cfg))
	since := time.Now()
	if err := installService(cfg, path); err != nil {
		return err
	}
	replaced.Store(true)
	done("后台服务已安装：%s", serviceFile(cfg))
	serviceNote(cfg)
	done("后台服务已启动，等它连上隧道……")
	reportTunnel(path, cfg, since)

	fmt.Println("\n完成。接下来：")
	for i, line := range nextSteps(cfg) {
		fmt.Printf("  %d. %s\n", i+1, line)
	}
	return nil
}

// guardInterrupt 接管 Ctrl-C：恢复终端，做 cleanup，再以 130 退出。返回的函数撤掉接管。
//
// 问密码时 ReadPassword 关掉了回显，要等读完才恢复；Ctrl-C 让进程直接退出，它来不及，
// 之后在这个终端里打字就看不见了。所以接管时先记下终端原来的状态，退出前替它恢复。
func guardInterrupt(cleanup func()) (stop func()) {
	fd := int(os.Stdin.Fd())
	saved, _ := term.GetState(fd) // 不是终端时为 nil
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	go func() {
		if _, ok := <-interrupted; ok {
			if saved != nil {
				_ = term.Restore(fd, saved)
			}
			fmt.Println()
			cleanup()
			os.Exit(130)
		}
	}()
	return func() {
		signal.Stop(interrupted)
		close(interrupted)
	}
}

// nextSteps 按模式给出装好之后该做的事。
func nextSteps(cfg *Config) []string {
	var lines []string
	switch cfg.Mode {
	case ModeProxy:
		lines = append(lines,
			fmt.Sprintf("在你的代理工具里，把 %s 转发到 socks5://%s", strings.Join(cfg.Domains, "、"), cfg.Listen),
			"在浏览器里打开学校网站试试")
	case ModeSystem:
		lines = append(lines, fmt.Sprintf("在浏览器里打开任意一个 %s 下的网站试试", cfg.Domains[0]))
	case ModeTUN:
		lines = append(lines, fmt.Sprintf("用任意应用访问 %s 下的网站或服务试试", cfg.Domains[0]))
	}
	return append(lines,
		"sidedoor status 查看隧道、流量和正在走 VPN 的连接",
		"sidedoor dashboard 打开网页面板，看实时流量和每条连接",
		"暂停用 sidedoor stop（撤掉代理，开机也不会自己连上），恢复用 sidedoor start；需要管理员权限时会在这里问你密码")
}

// reportTunnel 等后台服务连上隧道，再确认系统代理确实指向了它。
func reportTunnel(cfgPath string, cfg *Config, since time.Time) {
	rs, ok := waitTunnel(cfgPath, since, 30*time.Second)
	switch {
	case ok:
		done("隧道已连接，分到的地址 %s", strings.Join(rs.Address, ", "))
	case rs != nil:
		warn("隧道还没连上：%s。稍后用 sidedoor status 看看", rs.Message)
	default:
		warn("后台服务 30 秒内没写出状态，看看日志 %s", logFile(cfg))
	}
	if cfg.Mode == ModeSystem {
		if systemProxyIs(cfg.Listen) {
			done("系统代理已指向 %s；停止服务时会自动还原", cfg.Listen)
		} else {
			warn("系统代理没有指向 %s，可能被别的工具改了", cfg.Listen)
		}
	}
}

// waitTunnel 等后台服务把隧道连上，最多等 timeout。只认 since 之后写的状态，免得
// 读到旧进程留下的文件。
func waitTunnel(cfgPath string, since time.Time, timeout time.Duration) (*runtimeStatus, bool) {
	var last *runtimeStatus
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		rs, err := readStatus(statusPath(cfgPath))
		if err != nil || rs.Updated.Before(since) {
			continue
		}
		last = rs
		switch rs.State {
		case adapter.OpenConnectStateConnected:
			return rs, true
		case adapter.OpenConnectStateAuthPending, adapter.OpenConnectStateError:
			return rs, false
		}
	}
	return last, false
}

func prefixesText(info *adapter.OpenConnectTunnelInfo) string {
	if info == nil || len(info.IPv4) == 0 {
		return "（服务器没给）"
	}
	parts := make([]string, len(info.IPv4))
	for i, p := range info.IPv4 {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}

// askMode 让用户选模式：先说清楚系统代理现在的状况和每种模式会改什么，再给推荐。
// owner 是当前占着系统代理的地址，没人占时为空。
func askMode(in *bufio.Reader, listen, owner string) (string, error) {
	recommended := ModeSystem
	switch {
	case modeUnsupported(ModeSystem) != "":
		note("推荐 proxy；要让所有程序（包括不认代理设置的命令行程序）都能直接访问学校域名，用 tun。")
		recommended = ModeProxy
	case owner != "":
		note("检测到系统代理已被别的工具占用（%s）。选 system 会覆盖它，两个工具会来回抢，所以推荐 proxy。", owner)
		recommended = ModeProxy
	default:
		note("检测到系统代理没有被占用，推荐 system。")
	}
	var choices []adapter.OpenConnectAuthFormChoice
	for _, c := range []adapter.OpenConnectAuthFormChoice{
		{Value: ModeProxy, Label: "proxy   只开本地代理 " + listen + "，不改任何系统设置；要在你自己的代理工具里把学校域名转发过来"},
		{Value: ModeSystem, Label: "system  另外把系统代理指向它，浏览器直接生效；会修改系统代理设置，停止服务时自动还原"},
		{Value: ModeTUN, Label: "tun     虚拟网卡，所有应用都生效；要管理员权限，选了会在这里问你这台电脑的登录密码"},
	} {
		if modeUnsupported(c.Value) == "" {
			choices = append(choices, c)
		}
	}
	return askChoice(in, "可选的模式（* 是推荐项）", choices, recommended)
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
func login(cfg *Config, in *bufio.Reader) (credentials, *adapter.OpenConnectTunnelInfo, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	instance, err := newBox(ctx, map[string]any{
		"log":       map[string]any{"level": "error"},
		"dns":       map[string]any{"servers": []any{map[string]any{"type": "local", "tag": "local"}}},
		"endpoints": []any{endpointOptions(cfg, "")},
	})
	if err != nil {
		return credentials{}, nil, err
	}
	defer instance.Close()
	if err := instance.Start(); err != nil {
		return credentials{}, nil, err
	}
	ep, err := vpnEndpoint(instance)
	if err != nil {
		return credentials{}, nil, err
	}
	return answerLogin(ctx, ep, in)
}

// authEndpoint 是 answerLogin 用到的那几个 VPN 端点的方法，测试里换成假的服务器。
type authEndpoint interface {
	OpenConnectStatus() adapter.OpenConnectStatus
	StatusUpdated() <-chan struct{}
	CompleteAuthChallenge(challengeID string, response adapter.OpenConnectAuthResponse) error
	CancelAuthChallenge(challengeID string) error
}

// maxRejected 是 setup 里登录连续被拒多少次就停下。学校账号连续输错会被锁，后台服务的
// 密码被拒时也是为此不再重试。
const maxRejected = 3

// answerLogin 把服务器发来的每一份登录表单问用户、提交回去，直到隧道连上。服务器每拒绝
// 一次，就带着错误重发一份表单；被拒满 maxRejected 次就不再问。
func answerLogin(ctx context.Context, ep authEndpoint, in *bufio.Reader) (credentials, *adapter.OpenConnectTunnelInfo, error) {
	var creds credentials
	answered, seen := map[string]bool{}, map[string]bool{}
	rejected := 0
	for {
		updated := ep.StatusUpdated()
		st := ep.OpenConnectStatus()
		switch st.State {
		case adapter.OpenConnectStateConnected:
			return creds, st.TunnelInfo, nil
		case adapter.OpenConnectStateError:
			return creds, nil, fmt.Errorf("登录失败：%s", st.Error)
		case adapter.OpenConnectStateAuthPending:
			ch := st.AuthChallenge
			if ch == nil || answered[ch.ID] {
				break
			}
			answered[ch.ID] = true
			// 一次 setup 里被拒之间不会夹着登录成功（成功就结束了），所以累计次数就是服务器那边
			// 连续失败的次数；中间夹着不带错误的表单（比如单独问组的那一轮）也不清零。
			if ch.Error != "" {
				if rejected++; rejected >= maxRejected {
					_ = ep.CancelAuthChallenge(ch.ID)
					return creds, nil, fmt.Errorf("登录连续 %d 次被拒（服务器说：%s），先停下，免得账号被锁；确认用户名和密码后再运行 sidedoor setup", rejected, ch.Error)
				}
				warn("服务器提示：%s（还能再试 %d 次）", ch.Error, maxRejected-rejected)
			}
			values, err := askChallenge(ch, in, &creds, seen)
			if err != nil {
				_ = ep.CancelAuthChallenge(ch.ID)
				return creds, nil, err
			}
			response := adapter.OpenConnectAuthResponse{Form: &adapter.OpenConnectAuthFormResponse{Values: values}}
			if err := ep.CompleteAuthChallenge(ch.ID, response); err != nil {
				return creds, nil, fmt.Errorf("提交登录表单失败: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return creds, nil, errors.New("已取消")
		case <-updated:
		}
	}
}

// askChallenge 把一份登录表单逐项问用户，返回按提交键组织的答案。
// seen 记着已经显示过的服务器提示，跨轮共用。
func askChallenge(ch *adapter.OpenConnectAuthChallenge, in *bufio.Reader, creds *credentials, seen map[string]bool) (map[string]string, error) {
	if ch.Browser != nil {
		return nil, errors.New("服务器要求在浏览器里登录（SSO），sidedoor 暂不支持")
	}
	if ch.Form == nil {
		return nil, errors.New("服务器发来了一份空的登录表单")
	}
	for _, text := range []string{ch.Banner, ch.Message} {
		// 服务器每一轮表单都带着同一句提示，只说一遍。
		if text = strings.TrimSpace(text); text != "" && !seen[text] {
			seen[text] = true
			note("服务器说：%s", text)
		}
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
			note("%s: %s", label, value)
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

// defaultHinted 记着「方括号里是默认值」说过没有：只在第一次遇到默认值时说一遍。
var defaultHinted bool

// ask 提问并读一行；直接回车取默认值。
func ask(in *bufio.Reader, label, def string) (string, error) {
	if def != "" {
		if !defaultHinted {
			defaultHinted = true
			hint("方括号里是默认值，直接回车就用它")
		}
		fmt.Printf("    %s [%s]: ", label, def)
	} else {
		fmt.Printf("    %s: ", label)
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
	return readSecret(os.Stdin, os.Stdout, in, label)
}

// readSecret 是 askSecret 的实现，终端和提示的输出处可以换掉，方便测试。
//
// 先关回显再打印提示：ReadPassword 被调用时才关回显，要是先打印，看到提示就立刻输入的
// 程序（比如 expect 脚本）打的字会赶在回显关掉之前被回显出来。
func readSecret(tty *os.File, out io.Writer, in *bufio.Reader, label string) (string, error) {
	if fd := int(tty.Fd()); term.IsTerminal(fd) {
		restore, err := echoOff(fd)
		if err != nil {
			return "", err
		}
		defer restore()
		// 输入时什么都不显示，不说一声容易以为卡住了。
		fmt.Fprintf(out, "    %s（输入时不显示）: ", label)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(out)
		return string(b), err
	}
	fmt.Fprintf(out, "    %s: ", label)
	line, err := in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// askChoice 列出选项让用户选编号，直接回车取带 * 的那一项（服务器预选的，或推荐的）。
func askChoice(in *bufio.Reader, label string, options []adapter.OpenConnectAuthFormChoice, def string) (string, error) {
	if len(options) == 0 {
		return def, nil
	}
	note("%s", label)
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
		note("%s %d) %s", mark, i+1, name)
	}
	for {
		s, err := ask(in, "选择编号", strconv.Itoa(defIndex+1))
		if err != nil {
			return "", err
		}
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(options) {
			return options[n-1].Value, nil
		}
		warn("请输入 1 到 %d 之间的数字", len(options))
	}
}

// systemProxyOwner 返回当前系统代理指向的地址；没开系统代理，或者指向的就是
// listen，返回空串。目前只认 macOS。
func systemProxyOwner(listen string) string {
	m, ok := systemProxySettings()
	if !ok {
		return ""
	}
	return proxyOwner(m, listen)
}

// systemProxyIs 判断系统代理当前是否指向 listen。
func systemProxyIs(listen string) bool {
	m, ok := systemProxySettings()
	return ok && proxyIs(m, listen)
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

// proxyIs 看 HTTPS 代理：浏览器的流量绝大部分走它。
func proxyIs(m map[string]string, listen string) bool {
	return m["HTTPSEnable"] == "1" && net.JoinHostPort(m["HTTPSProxy"], m["HTTPSPort"]) == listen
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
	return fmt.Errorf("%s 已被占用：可能是另一种模式的 sidedoor 服务还在运行（先用 sidedoor stop 停掉），或者旧的 ocproxy 之类还开着: %w", listen, err)
}
