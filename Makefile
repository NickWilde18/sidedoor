# sing-box 的用户态协议栈放在 with_gvisor 编译标签后面，不带这个标签，VPN 端点
# 启动时会报「system:false requires the with_gvisor build tag」。
TAGS := with_gvisor
FLAGS := -tags $(TAGS) -trimpath -ldflags "-s -w"

.PHONY: build install test

build:
	go build $(FLAGS) -o sidedoor .

install:
	go install $(FLAGS) .

test:
	go test -tags $(TAGS) ./...
