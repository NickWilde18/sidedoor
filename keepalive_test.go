package main

import (
	"context"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/gvisor/pkg/buffer"
	"github.com/sagernet/gvisor/pkg/tcpip"
	"github.com/sagernet/gvisor/pkg/tcpip/adapters/gonet"
	"github.com/sagernet/gvisor/pkg/tcpip/header"
	"github.com/sagernet/gvisor/pkg/tcpip/link/channel"
	"github.com/sagernet/gvisor/pkg/tcpip/network/ipv4"
	"github.com/sagernet/gvisor/pkg/tcpip/stack"
	"github.com/sagernet/gvisor/pkg/tcpip/transport/tcp"
	"github.com/sagernet/sing-box/transport/openconnect"
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
)

var fakeServerAddr = tcpip.AddrFrom4([4]byte{10, 0, 0, 1})

// fakeServer 是隧道另一头的服务器：一个普通的 gVisor 协议栈，直接和 VPN 端点的网络栈收发 IP 包。
type fakeServer struct {
	stack *stack.Stack
	link  *channel.Endpoint
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	t.Cleanup(s.Close)
	link := channel.New(64, 1400, "")
	if err := s.CreateNIC(1, link); err != nil {
		t.Fatal(err)
	}
	address := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: fakeServerAddr.WithPrefix()}
	if err := s.AddProtocolAddress(1, address, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	return &fakeServer{stack: s, link: link}
}

// fakeTunnel 在 VPN 端点的网络栈和服务器之间转发 IP 包。attach 换上另一台服务器，
// 旧服务器之后发出的包（比如它的 FIN）一律丢掉，就像隧道断开时丢掉的那些。
type fakeTunnel struct {
	device openconnect.Device
	mu     sync.Mutex
	server *fakeServer
	stop   context.CancelFunc
}

func newFakeTunnel(t *testing.T, device openconnect.Device) *fakeTunnel {
	tn := &fakeTunnel{device: device}
	device.SetPacketWriter(func(packets []*buf.Buffer) error {
		tn.mu.Lock()
		server := tn.server
		tn.mu.Unlock()
		for _, packet := range packets {
			if server != nil {
				server.link.InjectInbound(ipv4.ProtocolNumber, stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(append([]byte(nil), packet.Bytes()...)),
				}))
			}
			packet.Release()
		}
		return nil
	})
	t.Cleanup(func() { tn.attach(nil) })
	return tn
}

func (tn *fakeTunnel) attach(server *fakeServer) {
	tn.mu.Lock()
	defer tn.mu.Unlock()
	if tn.stop != nil {
		tn.stop()
	}
	tn.server, tn.stop = server, nil
	if server == nil {
		return
	}
	ctx, stop := context.WithCancel(context.Background())
	tn.stop = stop
	go func() {
		for {
			pkt := server.link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			view := pkt.ToView()
			data := append([]byte(nil), view.AsSlice()...)
			view.Release()
			pkt.DecRef()
			if ctx.Err() != nil {
				return
			}
			_ = tn.device.WriteInboundBuffers([]*buf.Buffer{buf.As(data)})
		}
	}()
}

// TestVPNHalfClosedConnEndsWhenPeerVanishes 钉住「走 VPN 的连接，客户端先关之后，服务器的
// FIN 要是丢在了隧道里，这条连接就永远挂着」。真机上看到的两条 mail.cuhk.edu.cn:443 就是
// 这样：客户端那头早就关了，sing-box 的下载方向还卡在 gonet.(*TCPConn).Read 里等服务器，
// 一个挂了 2 天 5 小时，一个挂了 1 天 4 小时，状态里一直算作当前连接。
//
// 客户端读到 EOF 后，sing-box 只对 VPN 那侧做半关闭（route/conn.go:281 的 CloseWrite），然后
// 等对方也关。gVisor 对还没 Close 的连接不启动 FIN-WAIT-2 计时器，所以只有 keepalive 能发现
// 对方已经不在了。测试里 VPN 侧用的是 sing-box 真正用来拨 VPN 连接的那个网络栈
// （openconnect.NewDevice，system:false）；服务器收到 FIN 之后，换上一台不记得这条连接的
// 服务器，模拟它的 FIN 在隧道里丢了、之后也放弃了这条连接。
func TestVPNHalfClosedConnEndsWhenPeerVanishes(t *testing.T) {
	device, err := openconnect.NewDevice(openconnect.DeviceOptions{
		Context: context.Background(),
		MTU:     1400,
		Configuration: openconnect.Configuration{
			Addresses: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/32")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	if err := device.Start(); err != nil {
		t.Fatal(err)
	}
	tunnel := newFakeTunnel(t, device)
	server := newFakeServer(t)
	tunnel.attach(server)

	ln, err := gonet.ListenTCP(server.stack, tcpip.FullAddress{NIC: 1, Addr: fakeServerAddr, Port: 443}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 服务器：读完客户端的 1540 字节，回 99 字节（和真机上那两条的流量一样），然后一直读到客户端的 FIN。
	gotFIN := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, 1540)); err != nil {
			return
		}
		_, _ = conn.Write(make([]byte, 99))
		_, _ = io.Copy(io.Discard, conn)
		close(gotFIN)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := device.DialContext(ctx, "tcp", M.ParseSocksaddrHostPort("10.0.0.1", 443))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(make([]byte, 1540)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 99)); err != nil {
		t.Fatal(err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gotFIN:
	case <-time.After(10 * time.Second):
		t.Fatal("服务器 10 秒内没收到客户端的 FIN")
	}
	// 等服务器对 FIN 的 ACK 回到 VPN 侧，让 VPN 侧进入 FIN-WAIT-2，再换掉服务器。
	time.Sleep(500 * time.Millisecond)
	tunnel.attach(newFakeServer(t))

	readDone := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("服务器已经不在了，VPN 侧却读到了数据")
		}
	case <-time.After(40 * time.Second):
		t.Fatal("服务器消失 40 秒后，VPN 侧半关闭的连接还在等它的 FIN；sing-box 会一直挂着这条连接，goroutine、gVisor 端点和连接表里的那一行都不释放")
	}
}
