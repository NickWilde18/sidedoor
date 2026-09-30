# sidedoor

只让指定域名走学校的 VPN，其余流量照常直连。一个二进制，常驻后台，不用另装 OpenConnect、ocproxy 或别的代理软件。

连接 VPN（AnyConnect 兼容协议）、按域名分流、系统代理、虚拟网卡都由内嵌的 [sing-box](https://github.com/SagerNet/sing-box) 完成。sidedoor 自己只做三件事：把配置翻译成 sing-box 配置，保管密码（存在系统钥匙串里），把自己装成开机自启的后台服务。

## 安装

需要 Go 1.27 或更新的版本，但只在编译时需要。编译出来的二进制只依赖系统自带的库。

```sh
make install          # 装到 $(go env GOPATH)/bin/sidedoor
```

必须用 `make`：sing-box 的用户态协议栈放在 `with_gvisor` 编译标签后面，直接 `go build` 编出来的二进制启动时会报错。

## 使用

```sh
sidedoor setup        # 按提示登录一次，装好后台服务
sidedoor status       # 看后台服务和隧道状态
sidedoor stop / start / restart
sidedoor uninstall    # 卸载服务，删掉配置和保存的密码
```

`setup` 会问服务器地址，然后把服务器发来的登录表单原样问你（用户名、密码、组）。登录成功后，它会问哪些域名走 VPN，默认值是服务器的主域名，比如 `vpn.example.edu` 对应 `example.edu`。重复运行 `setup` 就是重新配置，改密码后也这样做。

## 三种模式

| 模式 | 做什么 | 谁生效 | 权限 |
|---|---|---|---|
| `proxy` | 只开本地代理 `127.0.0.1:11080`，SOCKS5 和 HTTP 共用一个端口 | 你自己指向它的应用或代理工具 | 普通用户 |
| `system` | 本地代理，外加把系统代理指向它 | 浏览器，以及遵守系统代理的应用 | 普通用户 |
| `tun` | 虚拟网卡，只接管要走 VPN 的域名 | 所有应用，包括 ssh | `sudo`，目前只支持 macOS |

不指定模式时，`setup` 会自动选：系统代理已经被别的工具占用（比如 Shadowrocket、Clash）就选 `proxy`，没被占用就选 `system`。指定模式用 `sidedoor setup -mode tun` 这种写法，其中 tun 模式要加 sudo。

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
| 配置（不含密码） | `~/.config/sidedoor/config.json`；tun 模式在 `/Library/Application Support/sidedoor/` |
| 密码 | 系统钥匙串，服务名 `sidedoor`；tun 模式存在只有 root 能读的 `/Library/Application Support/sidedoor/password` |
| 日志 | `~/Library/Logs/sidedoor/`；tun 模式在 `/Library/Logs/sidedoor/` |
| 后台任务 | `~/Library/LaunchAgents/com.nickwilde.sidedoor.plist`；tun 模式在 `/Library/LaunchDaemons/` |

## 限制

- 服务器要求在浏览器里登录（SSO/SAML）时，暂不支持。
- 如果服务器每次登录都要一次性验证码，断线重连时后台没法自动填，会停下来等你重新运行 `setup`。保存的密码被拒时也一样：后台不会拿旧密码反复重试，免得把账号锁掉。
- 经隧道只转发 TCP。浏览器访问学校网站时，HTTP/3（QUIC）走不通，会自动退回 TCP。
- tun 模式还没在真机上验证过。

## 开发

```sh
make test
```

sing-box 锁定在 v1.14.2。它的 Go 接口在小版本之间经常有破坏性改动，升级时先跑一遍 `make test`。

sing-box 采用 GPLv3 许可证，分发编译好的二进制时，需要按 GPLv3 同时提供源码。
