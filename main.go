// sidedoor 只让指定域名走 VPN，其余流量照常直连。
//
// 连接 VPN、分流、系统代理和虚拟网卡都交给 sing-box，这里只做三件事：把用户配置
// 翻译成 sing-box 配置、保管密码、把自己装成后台服务。
package main

import (
	"fmt"
	"os"
)

const usage = `sidedoor —— 只让指定域名走 VPN，其余流量照常直连

用法:
  sidedoor setup [-server 地址] [-mode proxy|system|tun] [-domains 后缀,...] [-listen 地址]
                       登录一次并装成后台服务；重复运行就是重新配置
  sidedoor status      查看隧道、流量和当前走 VPN 的连接
  sidedoor dashboard   打开网页面板，看实时流量和每条连接（第一次会从 GitHub 下载面板）
  sidedoor overlay     和别的代理软件一起用：生成在它里面导入一次就行的叠加层，订阅照常更新
  sidedoor start | stop | restart
                       启动、停止、重启后台服务
  sidedoor uninstall   卸载后台服务，删除配置和保存的密码
  sidedoor debug goroutines [-o 文件] | heap -o 文件
                       导出后台服务的 goroutine 栈或 heap profile，不用重启它
  sidedoor run [-config 路径] [-v]
                       在前台运行（后台服务就是这样启动的）

模式:
  proxy   只开本地代理（SOCKS5 和 HTTP 共用一个端口），分流交给你已有的代理工具
  system  本地代理加系统代理，浏览器等遵守系统代理的应用直接生效
  tun     虚拟网卡，所有应用都生效；需要 sudo，目前只支持 macOS
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "setup":
		err = cmdSetup(args)
	case "run":
		err = cmdRun(args)
	case "status":
		err = cmdStatus()
	case "dashboard":
		err = cmdDashboard()
	case "overlay":
		err = cmdOverlay()
	case "start", "stop", "restart":
		err = cmdControl(os.Args[1])
	case "uninstall":
		err = cmdUninstall()
	case "debug":
		err = cmdDebug(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, paint(colorErr, ansiRed, "sidedoor: "+err.Error()))
		os.Exit(1)
	}
}
