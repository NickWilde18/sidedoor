package main

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"
)

// traffic 按「走 VPN」和「直连」两条路分别累计流量和当前连接数。
//
// sing-box 自带的流量管理器（状态接口背后那个）只保留最近 1000 条已关闭的连接，
// 更早的并进一个不分出口的总数；它的连接关闭事件在缓冲满时还会被丢掉。要一直
// 把两条路分开算准，只能自己在每条连接上数字节。包装方式照抄它的 connTracker：
// 保留 Upstream，sing 走零拷贝转发时也照样计数。
type traffic struct {
	vpn, direct counter
}

type counter struct {
	up, down, active atomic.Int64
}

var _ adapter.ConnectionTracker = (*traffic)(nil)

func (t *traffic) counter(outbound adapter.Outbound) *counter {
	if outbound != nil && outbound.Tag() == "vpn" {
		return &t.vpn
	}
	return &t.direct
}

func (t *traffic) RoutedConnection(_ context.Context, conn net.Conn, _ adapter.InboundContext, _ adapter.Rule, outbound adapter.Outbound) net.Conn {
	c := t.counter(outbound)
	c.active.Add(1)
	return &countedConn{
		ExtendedConn: bufio.NewInt64CounterConn(conn, []*atomic.Int64{&c.up}, []*atomic.Int64{&c.down}),
		active:       &c.active,
	}
}

func (t *traffic) RoutedPacketConnection(_ context.Context, conn N.PacketConn, _ adapter.InboundContext, _ adapter.Rule, outbound adapter.Outbound) N.PacketConn {
	c := t.counter(outbound)
	c.active.Add(1)
	return &countedPacketConn{
		PacketConn: bufio.NewInt64CounterPacketConn(conn, []*atomic.Int64{&c.up}, nil, []*atomic.Int64{&c.down}, nil),
		active:     &c.active,
	}
}

// RoutedFlow 计 tun 模式下直接按包转发的流。
func (t *traffic) RoutedFlow(_ context.Context, _ adapter.InboundContext, _ adapter.Rule, outbound adapter.Outbound) tun.FlowTracker {
	return &countedFlow{c: t.counter(outbound)}
}

type countedConn struct {
	N.ExtendedConn
	active *atomic.Int64
	closed atomic.Bool
}

func (c *countedConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.active.Add(-1)
	}
	return c.ExtendedConn.Close()
}

func (c *countedConn) Upstream() any           { return c.ExtendedConn }
func (c *countedConn) ReaderReplaceable() bool { return true }
func (c *countedConn) WriterReplaceable() bool { return true }

type countedPacketConn struct {
	N.PacketConn
	active *atomic.Int64
	closed atomic.Bool
}

func (c *countedPacketConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.active.Add(-1)
	}
	return c.PacketConn.Close()
}

func (c *countedPacketConn) Upstream() any           { return c.PacketConn }
func (c *countedPacketConn) ReaderReplaceable() bool { return true }
func (c *countedPacketConn) WriterReplaceable() bool { return true }

type countedFlow struct {
	c    *counter
	open atomic.Bool
}

func (f *countedFlow) AttachFlow(tun.FlowHandle) {
	if f.open.CompareAndSwap(false, true) {
		f.c.active.Add(1)
	}
}

func (f *countedFlow) CountForward(n int) { f.c.up.Add(int64(n)) }
func (f *countedFlow) CountReverse(n int) { f.c.down.Add(int64(n)) }
func (f *countedFlow) FlowEstablished()   {}

func (f *countedFlow) CloseFlow(tun.FlowCloseReason) {
	if f.open.CompareAndSwap(true, false) {
		f.c.active.Add(-1)
	}
}

// routeStats 是一条路的流量快照。速率由后台每隔 statsInterval 用前后两次快照算出。
type routeStats struct {
	Upload       int64 `json:"upload"`
	Download     int64 `json:"download"`
	Active       int64 `json:"active"`
	UploadRate   int64 `json:"upload_rate"`   // 字节每秒
	DownloadRate int64 `json:"download_rate"` // 字节每秒
}

type trafficStats struct {
	VPN    routeStats `json:"vpn"`
	Direct routeStats `json:"direct"`
}

func (t *traffic) snapshot() trafficStats {
	return trafficStats{VPN: t.vpn.stats(), Direct: t.direct.stats()}
}

func (c *counter) stats() routeStats {
	return routeStats{Upload: c.up.Load(), Download: c.down.Load(), Active: c.active.Load()}
}

// withRates 用和上一次快照的差，算出这段时间里的每秒速率。
func (s trafficStats) withRates(prev trafficStats, elapsed time.Duration) trafficStats {
	rate := func(now, before int64) int64 {
		if elapsed <= 0 || now < before {
			return 0
		}
		return int64(float64(now-before) / elapsed.Seconds())
	}
	s.VPN.UploadRate = rate(s.VPN.Upload, prev.VPN.Upload)
	s.VPN.DownloadRate = rate(s.VPN.Download, prev.VPN.Download)
	s.Direct.UploadRate = rate(s.Direct.Upload, prev.Direct.Upload)
	s.Direct.DownloadRate = rate(s.Direct.Download, prev.Direct.Download)
	return s
}

// ratesFrom 沿用 r 里已经算好的速率，累计值保持不变。
func (s trafficStats) ratesFrom(r trafficStats) trafficStats {
	s.VPN.UploadRate, s.VPN.DownloadRate = r.VPN.UploadRate, r.VPN.DownloadRate
	s.Direct.UploadRate, s.Direct.DownloadRate = r.Direct.UploadRate, r.Direct.DownloadRate
	return s
}
