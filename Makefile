# sing-box 的用户态协议栈放在 with_gvisor 编译标签后面，不带这个标签，VPN 端点
# 启动时会报「system:false requires the with_gvisor build tag」。
TAGS := with_gvisor

# sing-box 的 OpenConnect 网络栈拨出的 TCP 没开 keepalive：客户端先关、服务器的 FIN 又丢在
# 隧道里时，这条连接永远挂着。patches/ 里的补丁照它自家 WireGuard 的写法补上 keepalive。
# Go 不允许用 -overlay 替换模块缓存里的文件，所以编译前把 sing-box 复制到 .build/、打上补丁，
# 再用一份加了 replace 的 go.mod（-modfile）编译，仓库里的 go.mod 不动。补丁只对
# PATCHED_SINGBOX 这个版本核对过；升级 sing-box 后先重新核对补丁，再改这个版本号。
PATCHED_SINGBOX := v1.14.2
SINGBOX_VERSION := $(shell go list -m -f '{{.Version}}' github.com/sagernet/sing-box)
PATCHES := $(wildcard patches/*.patch)
BUILD := .build
MODFILE := $(BUILD)/go.mod

FLAGS := -tags $(TAGS) -modfile $(MODFILE) -trimpath -ldflags "-s -w"

.PHONY: build install test check-singbox

build: $(MODFILE)
	go build $(FLAGS) -o sidedoor .

install: $(MODFILE)
	go install $(FLAGS) .

test: $(MODFILE)
	go test -tags $(TAGS) -modfile $(MODFILE) ./...

check-singbox:
	@if [ "$(SINGBOX_VERSION)" != "$(PATCHED_SINGBOX)" ]; then \
		echo "sing-box 是 $(SINGBOX_VERSION)，patches/ 里的补丁是给 $(PATCHED_SINGBOX) 做的：先核对补丁，再改 Makefile 里的 PATCHED_SINGBOX" >&2; \
		exit 1; \
	fi

$(BUILD)/sing-box/.patched: go.mod $(PATCHES) | check-singbox
	@rm -rf $(BUILD)/sing-box
	@mkdir -p $(BUILD)
	@go mod download github.com/sagernet/sing-box
	@cp -R "$$(go list -m -f '{{.Dir}}' github.com/sagernet/sing-box)" $(BUILD)/sing-box
	@chmod -R u+w $(BUILD)/sing-box
	@for p in $(PATCHES); do echo "打补丁 $$p"; patch -s -p1 -d $(BUILD)/sing-box < $$p || exit 1; done
	@touch $@

$(MODFILE): go.mod go.sum $(BUILD)/sing-box/.patched
	@cp go.mod $@
	@cp go.sum $(BUILD)/go.sum
	@printf '\nreplace github.com/sagernet/sing-box => %s\n' "$(CURDIR)/$(BUILD)/sing-box" >> $@
