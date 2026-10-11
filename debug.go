package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"time"
)

// 后台服务在配置目录开一个只有自己能读写的 Unix socket，上面挂 Go 自带的 pprof。不占
// TCP 端口，本机别的用户、浏览器里的网页（借 DNS rebinding）都碰不到它。连接挂着不放、
// 内存涨了的时候，用 sidedoor debug 当场导出 goroutine 栈或 heap profile，不用重启进程，
// 现场也就还在。

func debugSocketPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "debug.sock")
}

// serveDebug 在 path 上开 pprof，返回的函数关掉监听并删掉 socket 文件。
func serveDebug(path string) (func(), error) {
	// 进程被 kill 掉时来不及删 socket，留下的旧文件会让 Listen 失败。
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(ln) }()
	return func() {
		_ = server.Close()
		_ = os.Remove(path)
	}, nil
}

// fetchDebug 经调试 socket 取一份 pprof 数据，query 是 /debug/pprof/ 下的路径和参数。
func fetchDebug(socket, query string) (io.ReadCloser, error) {
	client := &http.Client{
		// heap profile 一般一秒内就出来；留余量给 goroutine 很多的时候。
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}
	resp, err := client.Get("http://sidedoor/debug/pprof/" + query)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("调试接口返回 %s", resp.Status)
	}
	return resp.Body, nil
}

func cmdDebug(args []string) error {
	if len(args) == 0 {
		return errors.New("要导出什么：sidedoor debug goroutines，或者 sidedoor debug heap -o 文件")
	}
	what := args[0]
	fs := flag.NewFlagSet("debug "+what, flag.ExitOnError)
	out := fs.String("o", "", "写到这个文件；goroutines 不给就打到终端")
	only := sideFlag(fs)
	_ = fs.Parse(args[1:])

	var query string
	switch what {
	case "goroutines":
		query = "goroutine?debug=2"
	case "heap":
		// heap profile 是二进制，打到终端没法看。
		if *out == "" {
			return errors.New("heap profile 是二进制文件，用 -o 指定存到哪，之后用 go tool pprof 打开")
		}
		query = "heap"
	default:
		return fmt.Errorf("不认识 %q，只能导出 goroutines 或 heap", what)
	}

	if switched, err := switchIfNeeded(*only); switched || err != nil {
		return err
	}
	path, _, err := loadSide(ownSide())
	if err != nil {
		return err
	}
	socket := debugSocketPath(path)
	body, err := fetchDebug(socket, query)
	if err != nil {
		return fmt.Errorf("连不上后台服务的调试接口 %s（服务没在运行，或者还是没有调试接口的旧版本）: %w", socket, err)
	}
	defer body.Close()

	if *out == "" {
		_, err = io.Copy(os.Stdout, body)
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	done("已写入 %s", *out)
	if what == "heap" {
		note("用 go tool pprof %s 查看", *out)
	}
	return nil
}
