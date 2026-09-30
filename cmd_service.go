package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kardianos/service"
)

// serviceName 是 launchd 任务的标签，也是 plist 和日志的文件名。
const serviceName = "com.nickwilde.sidedoor"

// newService 描述后台服务。tun 模式要建网卡，只能是 root 的系统级任务
// （/Library/LaunchDaemons）；其余模式是当前用户的 LaunchAgent，登录后自动启动。
// 进程退出后 launchd 会自动拉起（KeepAlive），断线重连由 sing-box 自己负责。
func newService(cfg *Config, cfgPath string) (service.Service, error) {
	return service.New(noop{}, &service.Config{
		Name:        serviceName,
		DisplayName: "sidedoor",
		Description: "只让指定域名走 VPN，其余流量照常直连",
		Arguments:   []string{"run", "-config", cfgPath},
		Option: service.KeyValue{
			"UserService":  cfg.Mode != ModeTUN,
			"RunAtLoad":    true,
			"KeepAlive":    true,
			"LogOutput":    true,
			"LogDirectory": logDir(cfg.Mode == ModeTUN),
		},
	})
}

// noop 满足 service.Interface。launchd 直接执行 sidedoor run，用不到它的回调。
type noop struct{}

func (noop) Start(service.Service) error { return nil }
func (noop) Stop(service.Service) error  { return nil }

func logDir(root bool) string {
	if root {
		return "/Library/Logs/sidedoor"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "sidedoor")
}

// installService 装好并启动后台服务。重复 setup 时先卸掉旧的再装，保证幂等。
func installService(cfg *Config, cfgPath string) error {
	// launchd 任务里记的是当前可执行文件的路径。go run 的临时文件过后就被删了，
	// 装上的任务会起不来。
	if exe, err := os.Executable(); err == nil && strings.Contains(exe, "go-build") {
		return errors.New("当前是 go run 的临时二进制，先 make install，再用装好的 sidedoor setup")
	}
	s, err := newService(cfg, cfgPath)
	if err != nil {
		return err
	}
	// launchd 不会替我们建日志目录，目录不存在时任务直接起不来。
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

// stopService 停掉已装的服务，没装就什么也不做。
func stopService(cfg *Config, cfgPath string) {
	if s, err := newService(cfg, cfgPath); err == nil {
		_ = s.Stop()
	}
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
	password, err := cfg.password()
	if err != nil {
		return err
	}
	// launchd 停止任务时发 SIGTERM，前台运行时是 Ctrl-C，都要走到 sing-box 的 Close，
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
	fmt.Printf("服务器   %s（%s）\n", cfg.Server, cfg.Username)
	fmt.Printf("走 VPN   %s\n", strings.Join(cfg.Domains, ", "))
	fmt.Printf("模式     %s，本地代理 %s\n", cfg.Mode, cfg.Listen)
	// 普通用户的 launchctl list 看不到 root 的系统级任务，会把运行中误报成已停止。
	if err := requireRoot(cfg); err != nil {
		return err
	}

	s, err := newService(cfg, path)
	if err != nil {
		return err
	}
	st, err := s.Status()
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		fmt.Println("后台服务 未安装，运行 sidedoor setup")
		return nil
	case err != nil:
		return err
	case st != service.StatusRunning:
		fmt.Println("后台服务 已停止，运行 sidedoor start")
		return nil
	}
	fmt.Println("后台服务 运行中")
	if rs, err := readStatus(statusPath(path)); err == nil {
		fmt.Printf("隧道     %s\n", rs.Message)
		if !rs.Since.IsZero() {
			fmt.Printf("连上于   %s\n", rs.Since.Local().Format("2006-01-02 15:04:05"))
		}
	} else {
		fmt.Println("隧道     还没有状态，可能正在启动")
	}
	fmt.Printf("日志     %s\n", logDir(cfg.Mode == ModeTUN))
	return nil
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
	return service.Control(s, action)
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
	if cfg.Mode == ModeTUN {
		tunDown(cfg)
	}
	if err := cfg.deletePassword(); err != nil {
		return fmt.Errorf("删除保存的密码: %w", err)
	}
	dir := filepath.Dir(path)
	for _, name := range []string{"config.json", "status.json", "cache.db"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	_ = os.Remove(dir) // 目录里还有别的东西就留着
	fmt.Printf("已卸载：后台服务、配置和保存的密码都已删除。日志留在 %s。\n", logDir(cfg.Mode == ModeTUN))
	return nil
}

// requireRoot 拦住用普通身份操作 tun 模式服务的情况：系统级任务只有 root 能动。
func requireRoot(cfg *Config) error {
	if cfg.Mode == ModeTUN && os.Geteuid() != 0 {
		return errors.New("tun 模式的服务是用 sudo 装的，这个操作也要加 sudo")
	}
	return nil
}
