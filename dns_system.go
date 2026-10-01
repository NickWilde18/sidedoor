package main

import (
	"context"
	"errors"
	"net"
	"strings"

	mDNS "github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/log"
)

// systemResolver 是操作系统的解析器。测试里会换成假的。
var systemResolver = net.DefaultResolver

// systemDNS 是一种 sing-box DNS 服务器：把 A/AAAA 查询交给操作系统的解析器，结果
// 和其他应用看到的一样。直连流量都用它解析。
//
// sing-box 自带的 local 在 macOS 命令行下只问默认网卡的那组 DNS（它的
// systemconfig/source_darwin.go 里的 build），不认按域名分流的解析器：Tailscale 的
// MagicDNS、别的 VPN 的内网域名、/etc/resolver 里的配置。经 sidedoor 直连时这些名字
// 就解析不出来，浏览器只能拿到 502。Go 在 macOS 上走 cgo，最终由 mDNSResponder 解析，
// 这些它都认。
type systemDNS struct {
	dns.TransportAdapter
}

type systemDNSOptions struct{}

const (
	systemDNSType = "system"
	// systemDNSTTL 是给 sing-box 缓存用的有效期。系统解析器不告诉我们真实的 TTL，
	// 取一个短的；它自己也有缓存，重复查询不费事。
	systemDNSTTL = 30
)

func registerSystemDNS(registry *dns.TransportRegistry) {
	dns.RegisterTransport[systemDNSOptions](registry, systemDNSType,
		func(_ context.Context, _ log.ContextLogger, tag string, _ systemDNSOptions) (adapter.DNSTransport, error) {
			return &systemDNS{TransportAdapter: dns.NewTransportAdapter(systemDNSType, tag, nil)}, nil
		})
}

func (*systemDNS) Start(adapter.StartStage) error { return nil }
func (*systemDNS) Close() error                   { return nil }
func (*systemDNS) Reset()                         {}

func (t *systemDNS) ExchangeAsync(ctx context.Context, message *mDNS.Msg, callback func(*mDNS.Msg, error)) {
	go func() { callback(t.Exchange(ctx, message)) }()
}

// Exchange 只回答 A 和 AAAA：直连只需要地址。别的类型回一个空的成功响应。
func (t *systemDNS) Exchange(ctx context.Context, message *mDNS.Msg) (*mDNS.Msg, error) {
	response := new(mDNS.Msg)
	response.SetReply(message)
	question := message.Question[0]
	var network string
	switch question.Qtype {
	case mDNS.TypeA:
		network = "ip4"
	case mDNS.TypeAAAA:
		network = "ip6"
	default:
		return response, nil
	}
	addrs, err := systemResolver.LookupNetIP(ctx, network, strings.TrimSuffix(question.Name, "."))
	if err != nil {
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			return nil, err
		}
		// 系统解析器分不清「没有这个名字」和「名字在，但没有这种记录」。只在查 A 时
		// 报 NXDOMAIN；查 AAAA 查不到，多半只是它没有 IPv6 地址，回空就行。
		if question.Qtype == mDNS.TypeA {
			response.Rcode = mDNS.RcodeNameError
		}
		return response, nil
	}
	for _, addr := range addrs {
		header := mDNS.RR_Header{Name: question.Name, Rrtype: question.Qtype, Class: mDNS.ClassINET, Ttl: systemDNSTTL}
		if addr = addr.Unmap(); addr.Is4() {
			response.Answer = append(response.Answer, &mDNS.A{Hdr: header, A: addr.AsSlice()})
		} else {
			response.Answer = append(response.Answer, &mDNS.AAAA{Hdr: header, AAAA: addr.AsSlice()})
		}
	}
	return response, nil
}
