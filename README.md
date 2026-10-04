# sidedoor

只让指定域名走学校的 VPN，其余流量照常直连。一个二进制，常驻后台，不用另装 OpenConnect、ocproxy 或别的代理软件。

连接 VPN（AnyConnect 兼容协议）、按域名分流、系统代理、虚拟网卡、连接统计都由内嵌的 [sing-box](https://github.com/SagerNet/sing-box) 完成。sidedoor 自己只做几件事：把配置翻译成 sing-box 配置，保管密码，按"走 VPN / 直连"分别统计流量，把自己装成开机自启的后台服务。

## 安装

需要 Go 1.27 或更新的版本，但只在编译时需要。编译出来的二进制只依赖系统自带的库。

```sh
make install          # 装到 $(go env GOPATH)/bin/sidedoor
```

必须用 `make`：sing-box 的用户态协议栈放在 `with_gvisor` 编译标签后面，直接 `go build` 编出来的二进制启动时会报错；make 还会给 sing-box 打上 `patches/` 里的补丁（见[开发](#开发)）。

## 使用

```sh
sidedoor setup        # 按提示一步步来：选模式、登录、选域名，装好后台服务
sidedoor status       # 隧道状态、流量、当前走 VPN 的连接
sidedoor dashboard    # 网页面板：实时流量和每一条连接
sidedoor stop / start / restart
sidedoor uninstall    # 卸载服务，删掉配置和保存的密码
```

`setup` 分五步，每一步做了什么、改了哪些文件和系统设置都会打出来：

1. **检查环境**：停掉正在运行的旧服务，确认端口空闲。之后要是失败或者被 Ctrl-C 打断，会把旧服务恢复回去。
2. **选择模式**：先告诉你系统代理现在有没有被别的工具占着，再列出三种模式和各自会改什么，按情况标出推荐项。
3. **登录**：把服务器发来的登录表单原样问你，比如组、用户名、密码。登录成功说明账号没问题，随即断开。
4. **选域名**：哪些域名走 VPN，默认是服务器的主域名，比如 `vpn.example.edu` 对应 `example.edu`。
5. **保存并启动**：写配置、存密码、装后台服务，然后等它真正连上隧道，最后给出接下来该做什么。

重复运行 `setup` 就是重新配置，改密码后也这样做。

## 三种模式

| 模式 | 做什么 | 谁生效 | 权限 |
|---|---|---|---|
| `proxy` | 只开本地代理 `127.0.0.1:11080`，SOCKS5 和 HTTP 共用一个端口，不改任何系统设置 | 你自己指向它的应用或代理工具 | 普通用户 |
| `system` | 本地代理，外加把系统代理指向它；停止服务时自动还原 | 浏览器，以及遵守系统代理的应用 | 普通用户 |
| `tun` | 虚拟网卡，只接管要走 VPN 的域名 | 所有应用，包括 ssh | `sudo`，目前只支持 macOS |

系统代理已经被别的工具占着（比如 Shadowrocket、Clash）时推荐 `proxy`，否则推荐 `system`。不想交互的话可以直接指定，比如 `sidedoor setup -mode proxy`，tun 模式要用 `sudo sidedoor setup -mode tun`。

### 已经在用代理工具（proxy 模式）

在工具里把要走 VPN 的域名转发到 `socks5://127.0.0.1:11080`，以 Shadowrocket 为例：

```
[Proxy]
VPN = socks5, 127.0.0.1, 11080

[Rule]
DOMAIN-SUFFIX,example.edu,VPN
FINAL,DIRECT
```

如果之前用的是 OpenConnect 加 ocproxy，端口本来就是 11080，原来的规则不用改，关掉那两个程序就行。

## 状态和流量

```
$ sidedoor status
sidedoor 状态
  服务器     vpn.example.edu（alice，组 Staff）
  走 VPN     example.edu
  模式       system：本地代理 127.0.0.1:11080，系统代理指向它
  后台服务   运行中（PID 4242），已运行 2 小时 5 分钟
  隧道       已连接 2 小时 5 分钟（14:03 起），地址 10.0.17.163
  DNS        10.0.0.53（学校域名经隧道问它）
  系统代理   已指向 127.0.0.1:11080
  流量       走 VPN  ↑ 1.2 MiB   ↓ 35 MiB    当前 3 个连接，速率 ↑ 2.0 KiB/s ↓ 120 KiB/s
             直连    ↑ 5.1 MiB   ↓ 210 MiB   当前 18 个连接，速率 ↑ 0 B/s ↓ 30 KiB/s
  VPN 连接   当前 3 条
              1. portal.example.edu:443              ↓ 2.1 MiB   ↑ 40 KiB    已 3 分钟
              ...
```

- **流量**由 sidedoor 自己在每条连接上计数，按"走 VPN / 直连"分开累计，从后台服务启动时算起。
- **VPN 连接**来自 sing-box 的状态接口：只监听 `127.0.0.1:11081`，访问需要密钥，密钥在配置文件里。
- **网页面板**（`sidedoor dashboard`）用的是现成的 Yacd 面板，能看实时流量曲线和每一条连接。面板文件要第一次运行这个命令时才从 GitHub 下载，所以默认不开，平时启动后台服务不依赖 GitHub。命令会带着后台地址和密钥打开面板，面板直接连上，不用手动填。

## 和其他 VPN、TUN 共存

- **proxy 和 system 模式不建虚拟网卡**，VPN 的 IP 包只在进程内部的用户态协议栈里流转，不改路由表，也不改系统 DNS，所以不会和别的 VPN 或 TUN 冲突。
- **tun 模式只占一小块地址**：
  - 不接管默认路由，只把假 IP 段 `198.51.100.0/24`（RFC 5737 文档保留网段）路由进自己的网卡。按最长前缀匹配，其他流量仍然走原来的路由。
  - 不改系统 DNS，只为每个域名后缀写一份 `/etc/resolver/<后缀>`，macOS 只会把这些域名的查询发过来。
  - 启动前会检查假 IP 段有没有被别的网卡接管，被接管了就拒绝启动。退出时删掉自己写的 resolver 文件，网卡和路由随进程一起消失。
  - 开启了"包含所有网络"（强制所有流量进它的隧道）的 VPN 应用和任何 TUN 都无法共存，遇到这种情况请用 proxy 模式。
- **旧的 OpenConnect 还连着时先关掉它**：服务器上同时挂着两个会话，可能会互相踢下线。

## 文件在哪

| 内容 | 位置 |
|---|---|
| 配置（不含密码，含状态接口的密钥，权限 0600） | `~/.config/sidedoor/config.json`；tun 模式在 `/Library/Application Support/sidedoor/` |
| 密码 | 系统钥匙串，服务名 `sidedoor`；tun 模式存在只有 root 能读的 `/Library/Application Support/sidedoor/password` |
| 日志 | `~/Library/Logs/sidedoor/com.nickwilde.sidedoor.err.log`；tun 模式在 `/Library/Logs/sidedoor/`。超过 10 MB 时只保留最后 1 MB |
| 后台任务 | `~/Library/LaunchAgents/com.nickwilde.sidedoor.plist`；tun 模式在 `/Library/LaunchDaemons/` |
| 网页面板文件 | 配置目录下的 `ui/` |

## 限制

- 服务器要求在浏览器里登录（SSO/SAML）时，暂不支持。
- 如果服务器每次登录都要一次性验证码，断线重连时后台没法自动填，会停下来等你重新运行 `setup`。保存的密码被拒时也一样：后台不会拿旧密码反复重试，免得把账号锁掉。
- **system 模式下，所有遵守系统代理的应用都经过 sidedoor**，所以：
  - sidedoor 进程意外退出到被 launchd 拉起之间（实测约 8 秒），这些应用都连不上网。
  - 直连流量的 DNS 用的是系统配置的解析器。系统解析器出问题时（比如睡眠唤醒后 Tailscale 的 MagicDNS 几分钟不回应），这些应用的连接也会跟着失败，日志里会出现大量 `context deadline exceeded`。这不是 sidedoor 本身的问题，系统里不经过代理的应用同样会受影响。
- 经本地代理转发的 UDP（SOCKS5 UDP）没有验证过。
- tun 模式还没在真机上验证过。

## 开发

```sh
make test
```

sing-box 锁定在 v1.14.2。它的 Go 接口在小版本之间经常有破坏性改动，升级时先跑一遍 `make test`。

`patches/` 里是给 sing-box 打的补丁：它的 OpenConnect 网络栈拨出的 TCP 没开 keepalive，客户端先关、服务器的 FIN 又丢在隧道里时，连接会永远挂着。Go 不允许用 `-overlay` 替换模块缓存里的文件，所以 make 先把 sing-box 复制到 `.build/`、打上补丁，再用一份加了 replace 的 go.mod（`-modfile`）编译，仓库里的 go.mod 不动。直接 `go test` 用的是没打补丁的 sing-box，`TestVPNHalfClosedConnEndsWhenPeerVanishes` 会失败。升级 sing-box 时先核对补丁，再改 Makefile 里的 `PATCHED_SINGBOX`；版本对不上，make 会直接报错。

## 许可证

Copyright (C) 2026 Yechi Yang

sidedoor 以 GPLv3 发布，全文见 [LICENSE](LICENSE)。
