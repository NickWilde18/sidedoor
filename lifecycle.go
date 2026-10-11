package main

// 这个文件管两件事：这台机器上装的是哪种模式，以及由哪个身份去操作它。
//
// 三种模式装在两个地方（这里叫两「边」）：tun 模式的服务以 root 运行，配置在系统目录；
// proxy 和 system 模式是当前用户的后台服务，配置在 ~/.config/sidedoor，密码在用户的钥匙串。
// 同一时刻只允许装一种：两边的服务监听同一个端口，抢不到的那个会在 KeepAlive 下反复重启。
// 因为只会有一份配置，配置在哪一边就说明是哪种模式，不另外记「当前模式」。
//
// 每个命令都先看两边装了什么，再按 planControl / planSetup 排出步骤。不在当前身份这一边
// 的步骤，切换身份运行 sidedoor 自己去做（runSwitched）：普通用户经 sudo 变成 root，在同一个
// 终端里输密码；root 回到运行 sudo 的那个用户，进他的登录会话。切换后的进程带着内部参数
// -side，只操作那一边，不再切换。

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"time"
)

// side 是一种模式装在哪一边。
type side string

const (
	sideUser side = "user" // proxy 和 system：当前用户的后台服务
	sideTUN  side = "tun"  // tun：root 的系统级服务
)

// sideOf 返回 mode 装在哪一边。
func sideOf(mode string) side {
	if mode == ModeTUN {
		return sideTUN
	}
	return sideUser
}

// currentEUID 是当前进程的有效用户 ID。测试里换成假的，模拟 root 和普通用户。
var currentEUID = os.Geteuid

// ownSide 是当前身份能直接操作的那一边：root 管 tun，普通用户管 proxy 和 system。
func ownSide() side {
	if currentEUID() == 0 {
		return sideTUN
	}
	return sideUser
}

// lookupUser 按用户名查用户。测试里换成假的。
var lookupUser = user.Lookup

// sideDir 返回 s 那一边的配置目录。
//
// root 看普通模式那一边时，用运行 sudo 的那个用户（SUDO_USER）的目录，而不是 HOME：macOS
// 的 sudo 保留 HOME、Linux 的不保留，按 HOME 算两个平台会不一样。不是经 sudo 运行，或者查不到
// 这个用户时返回空串。
func sideDir(s side) string {
	if s == sideTUN {
		return configDir(true)
	}
	if currentEUID() != 0 {
		return configDir(false)
	}
	name := os.Getenv("SUDO_USER")
	if name == "" || name == "root" {
		return ""
	}
	u, err := lookupUser(name)
	if err != nil {
		return ""
	}
	return filepath.Join(u.HomeDir, ".config", "sidedoor")
}

// sideConfigPath 返回 s 那一边的 config.json，不知道在哪时返回空串。
func sideConfigPath(s side) string {
	dir := sideDir(s)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.json")
}

// installedSides 按 user、tun 的顺序返回装着模式的那几边。配置文件在就算装着：只 stat 不读，
// 普通用户也能判断 root 那边装没装。
func installedSides() []side {
	var sides []side
	for _, s := range []side{sideUser, sideTUN} {
		if path := sideConfigPath(s); path != "" {
			if _, err := os.Stat(path); err == nil {
				sides = append(sides, s)
			}
		}
	}
	return sides
}

// step 是命令要做的一步：在 Side 那一边做 Cmd。Local 为真时当前身份直接做，否则切换到
// Side 那一边的身份去做。
type step struct {
	Side  side
	Cmd   string
	Local bool
}

// setupPlan 是 setup 的步骤：先停掉另一边的旧模式，再装新模式；装好后卸掉旧模式，没装成就
// 把旧模式重新启动。
//
// 旧模式和新模式在同一边时不进这几个列表：setup 本来就会先停同一边的旧服务、失败时恢复它。
type setupPlan struct {
	Stop    []step
	Install step
	Cleanup []step
	Restore []step
}

// planSetup 排出在 target 那一边装新模式的步骤。own 是当前身份那一边，installed 是装着的那几边。
func planSetup(own, target side, installed []side) setupPlan {
	p := setupPlan{Install: step{Side: target, Cmd: "setup", Local: own == target}}
	for _, s := range installed {
		if s == target {
			continue
		}
		local := s == own
		p.Stop = append(p.Stop, step{Side: s, Cmd: "stop", Local: local})
		p.Cleanup = append(p.Cleanup, step{Side: s, Cmd: "uninstall", Local: local})
		p.Restore = append(p.Restore, step{Side: s, Cmd: "start", Local: local})
	}
	return p
}

// onlyRunning 从 steps 里只留下 running 判为运行中的那几边。setup 没装成时只恢复本来在跑、
// 被 setup 停掉的旧模式；本来就停着的不去启动它。
func onlyRunning(steps []step, running func(side) bool) []step {
	var kept []step
	for _, st := range steps {
		if running(st.Side) {
			kept = append(kept, st)
		}
	}
	return kept
}

// sideRunning 判断 s 那一边的后台服务在不在跑：配置目录里有活着的进程写的状态文件。两种身份
// 都读得到这个文件，所以不用切换身份。
func sideRunning(s side) bool {
	path := sideConfigPath(s)
	if path == "" {
		return false
	}
	_, err := currentStatus(statusPath(path))
	return err == nil
}

// planControl 排出 start、stop、restart、uninstall 的步骤。
//
// stop 和 uninstall 对每个装着的模式各做一次：旧版本可能留下两种都装着的局面，要能一次清干净。
// start 和 restart 只认唯一装着的那一种。restart 就是 stop 再 start。
func planControl(cmd string, own side, installed []side) ([]step, error) {
	if len(installed) == 0 {
		return nil, errors.New("还没有配置，先运行 sidedoor setup")
	}
	switch cmd {
	case "stop", "uninstall":
		steps := make([]step, 0, len(installed))
		for _, s := range installed {
			steps = append(steps, step{Side: s, Cmd: cmd, Local: s == own})
		}
		return steps, nil
	case "start", "restart":
		if len(installed) > 1 {
			return nil, errBothInstalled
		}
		s := installed[0]
		local := s == own
		if cmd == "restart" {
			return []step{{Side: s, Cmd: "stop", Local: local}, {Side: s, Cmd: "start", Local: local}}, nil
		}
		return []step{{Side: s, Cmd: "start", Local: local}}, nil
	}
	return nil, fmt.Errorf("不认识 %q", cmd)
}

// errBothInstalled 表示两种模式都装着，只会是旧版本留下的。
var errBothInstalled = errors.New("tun 模式和普通模式都装着（旧版本留下的），先运行 sidedoor uninstall 全部卸掉，再重新运行 sidedoor setup")

// onlySide 给只操作一种模式的命令（dashboard、overlay、debug）找到装着的那一边。
func onlySide() (side, error) {
	switch installed := installedSides(); len(installed) {
	case 0:
		return "", errors.New("还没有配置，先运行 sidedoor setup")
	case 1:
		return installed[0], nil
	default:
		return "", errBothInstalled
	}
}

// sideFlag 注册内部参数 -side。它由 sidedoor 切换身份时自己带上，用户不用管。
func sideFlag(fs *flag.FlagSet) *string {
	return fs.String("side", "", "内部参数：只操作这一边（user 或 tun），由 sidedoor 切换身份时自己带上")
}

// parseSide 检查 -side 的值。没给时返回空串；给了就必须是当前身份那一边，否则说明切换身份
// 没切成，不能接着做。
func parseSide(v string) (side, error) {
	switch s := side(v); s {
	case "":
		return "", nil
	case sideUser, sideTUN:
		if s != ownSide() {
			return "", fmt.Errorf("-side %s 要以%s身份运行", s, map[side]string{sideUser: "普通用户", sideTUN: " root "}[s])
		}
		return s, nil
	}
	return "", fmt.Errorf("-side 只能是 user 或 tun，不是 %q", v)
}

// switchCommand 拼出切换到 target 那一边的身份运行 exe args... 的命令。
//
// 去 tun 那边：sudo，密码在同一个终端里输。回普通模式那边：要进运行 sudo 的那个用户的登录会话
// （钥匙串、LaunchAgent、systemctl --user 都认会话，光改进程身份进不去），并用他的 HOME，
// 各平台的写法见 asUserCommand。查不到是哪个用户时报错，让用户别用 sudo 运行。
func switchCommand(target side, exe string, args []string) ([]string, error) {
	if target == sideTUN {
		return append([]string{"sudo", exe}, args...), nil
	}
	name, uid := os.Getenv("SUDO_USER"), os.Getenv("SUDO_UID")
	if name == "" || name == "root" || uid == "" {
		return nil, errors.New("proxy 和 system 模式装在普通用户自己名下，查不到是哪个用户运行的 sudo，请不要用 sudo 运行")
	}
	return asUserCommand(uid, name, append([]string{exe}, args...)), nil
}

// errReported 表示切换身份后运行的那个 sidedoor 失败了，错误它已经打出来了，不用再打一遍。
var errReported = errors.New("切换身份后运行的那一步没有完成")

// runSwitched 切换到 target 那一边的身份运行 sidedoor args...，终端照常连着：要输的密码、
// setup 要问的问题、输出都在这个终端里。测试里换成假的。
//
// 子进程运行期间，Ctrl-C 只交给子进程：它会自己收尾、恢复它那边的旧服务；这边要等它退出后
// 接着把另一边的旧模式恢复回来，不能跟着一起退出。
var runSwitched = func(target side, args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	argv, err := switchCommand(target, exe, args)
	if err != nil {
		return err
	}
	if target == sideTUN {
		note("tun 模式要管理员权限，下面用 sudo 接着做；要输入的是这台电脑的登录密码")
	}
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	defer signal.Stop(interrupted)

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return errReported
		}
		return fmt.Errorf("运行 %s: %w", argv[0], err)
	}
	return nil
}

// runSteps 依次做完 steps，遇到失败就停。
func runSteps(steps []step) error {
	for _, st := range steps {
		if err := runStep(st); err != nil {
			return err
		}
	}
	return nil
}

func runStep(st step) error {
	if !st.Local {
		return runSwitched(st.Side, st.Cmd, "-side", string(st.Side))
	}
	switch st.Cmd {
	case "stop":
		return stopSide(st.Side)
	case "start":
		return startSide(st.Side)
	case "uninstall":
		return uninstallSide(st.Side)
	}
	return fmt.Errorf("不认识 %q", st.Cmd)
}

// loadSide 读当前身份这一边的配置。root 读到旧版本写的 tun 配置（只有 root 能读、密钥混在
// 里面）时顺手按新格式重写，之后普通用户的 status 就能读了。
func loadSide(s side) (string, *Config, error) {
	path := sideConfigPath(s)
	if path == "" {
		return "", nil, errors.New("查不到是哪个用户运行的 sudo，请不要用 sudo 运行")
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return "", nil, err
	}
	if err := upgradeTunConfig(path, cfg); err != nil {
		return "", nil, err
	}
	return path, cfg, nil
}

// upgradeTunConfig 把旧版本写的 tun 配置按新格式重写：密钥挪进 api_secret，config.json
// 改成所有人可读。不是 tun 模式、或者已经是新格式时什么都不做。
func upgradeTunConfig(path string, cfg *Config) error {
	if cfg.Mode != ModeTUN || currentEUID() != 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() == tunConfigPerm {
		return nil
	}
	cfg.fillDefaults()
	return cfg.save(path)
}

// svcState 是后台服务的状态，按文件判断，见 serviceState。
type svcState int

const (
	stateStopped  svcState = iota // 任务文件不在：stop 过，或者从来没 start
	stateStarting                 // 任务文件在，还没有活着的进程写出状态
	stateRunning                  // 任务文件在，有活着的进程写的状态
)

func (s svcState) String() string {
	return [...]string{"已停止", "正在启动", "运行中"}[s]
}

// serviceState 按任务文件和状态文件判断后台服务的状态。
//
// 不问 launchctl：普通用户的 launchctl list 看不到 root 的系统级任务。stop 会删掉任务文件，
// 所以任务文件在不在就是「停没停」；任务文件在、但状态文件没有或者是死掉的进程留下的，
// 说明正在启动，或者刚退出、正被重新拉起来。
func serviceState(cfg *Config, path string) (svcState, *runtimeStatus) {
	if _, err := os.Stat(serviceFile(cfg)); err != nil {
		return stateStopped, nil
	}
	rs, err := currentStatus(statusPath(path))
	if err != nil {
		return stateStarting, nil
	}
	return stateRunning, rs
}

// stopSide 停掉当前身份这一边的后台服务：卸掉任务文件，撤掉系统代理和分流 DNS，配置和密码留着。
//
// 只 unload 不删任务文件的话，任务文件带着 RunAtLoad，下次开机或登录又会自动连上；
// 所以 stop 直接卸掉任务文件，start 时按配置重新生成。
func stopSide(s side) error {
	path, cfg, err := loadSide(s)
	if err != nil {
		return err
	}
	file := serviceFile(cfg)
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		done("%s 模式的后台服务本来就是停着的", cfg.Mode)
	} else {
		svc, err := newService(cfg, path)
		if err != nil {
			return err
		}
		switch err := uninstallService(svc, file); {
		case errors.Is(err, errServiceGone):
			warn("%v，接着清理剩下的", err)
		case err != nil:
			return fmt.Errorf("停止后台服务失败，%s 还在: %w", file, err)
		default:
			done("%s 模式的后台服务已停止，任务文件 %s 已移除，开机和登录后都不会自动连上", cfg.Mode, file)
		}
	}
	_ = os.Remove(statusPath(path))
	switch cfg.Mode {
	case ModeSystem:
		reportProxyRestored(cfg.Listen)
	case ModeTUN:
		tunDown(cfg)
		done("%s", tunCleanupText())
	}
	return nil
}

// startSide 按当前身份这一边的配置装上并启动后台服务，等它连上隧道。不用重新 setup。
func startSide(s side) error {
	path, cfg, err := loadSide(s)
	if err != nil {
		return err
	}
	if st, rs := serviceState(cfg, path); st == stateRunning {
		done("%s 模式的后台服务已经在运行（PID %d）", cfg.Mode, rs.PID)
		return nil
	}
	if _, err := cfg.password(); err != nil {
		return err
	}
	since := time.Now()
	if err := installService(cfg, path); err != nil {
		return err
	}
	done("%s 模式的后台服务已启动（%s），等它连上隧道……", cfg.Mode, serviceFile(cfg))
	reportTunnel(path, cfg, since)
	return nil
}

// uninstallSide 卸掉当前身份这一边：先 stop，再删保存的密码和配置目录里 sidedoor 自己的文件。
// 日志留着，出了问题还能翻。
func uninstallSide(s side) error {
	if err := stopSide(s); err != nil {
		return err
	}
	path, cfg, err := loadSide(s)
	if err != nil {
		return err
	}
	if err := cfg.deletePassword(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("删除保存的密码: %w", err)
	}
	done("已删除保存的密码（%s）", passwordPlace(cfg))
	dir := filepath.Dir(path)
	for _, name := range []string{"config.json", apiSecretFile, "password", "status.json", "cache.db", "debug.sock"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	_ = os.RemoveAll(filepath.Join(dir, "ui"))
	_ = os.RemoveAll(overlayDir(path))
	_ = os.Remove(dir) // 目录里还有别的东西就留着
	done("已删除 %s 模式的配置 %s", cfg.Mode, dir)
	note("日志留在 %s，不需要的话可以手动删掉", logDir(cfg.Mode == ModeTUN))
	return nil
}

// cmdControl 是 start、stop、restart、uninstall。
func cmdControl(action string, args []string) error {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	only := sideFlag(fs)
	_ = fs.Parse(args)
	s, err := parseSide(*only)
	if err != nil {
		return err
	}
	if s != "" {
		if action == "restart" {
			return runSteps([]step{{Side: s, Cmd: "stop", Local: true}, {Side: s, Cmd: "start", Local: true}})
		}
		return runStep(step{Side: s, Cmd: action, Local: true})
	}
	steps, err := planControl(action, ownSide(), installedSides())
	if err != nil {
		return err
	}
	return runSteps(steps)
}

// switchIfNeeded 给只操作一种模式的命令（dashboard、overlay、debug）用：装着的那一边不归当前
// 身份管时，切换身份把整条命令重跑一遍，返回 true；归当前身份管时返回 false，由调用方接着做。
func switchIfNeeded(only string) (switched bool, err error) {
	s, err := parseSide(only)
	if err != nil || s != "" {
		return false, err
	}
	if s, err = onlySide(); err != nil {
		return false, err
	}
	if s == ownSide() {
		return false, nil
	}
	return true, runSwitched(s, append(slices.Clone(os.Args[1:]), "-side", string(s))...)
}
