package plugin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// 出站地址检查：只许连公网。
//
// fetch_url 的地址可能是模型从一页被投毒的网页里抄来的，也可能是对话里别人贴的。
// 放它去连 127.0.0.1 或 169.254.169.254，等于把本机服务与云厂商的元数据接口交给一段
// 第三方文字，而那段文字接下来还会被念进对话里。搜索插件让这一步更近了：模型现在有
// 一个「自己找链接」的入口。
//
// 两层，各管一半：
//
//   - BlockedHost 看 URL 里写死的地址。不查 DNS，因此对代理、对解析不了的名字都不会
//     误伤，代价是看不见「域名解析到内网」这一种。
//   - GuardedTransport 在拨号那一刻看真正要连的 IP。域名解析、302 跳到内网、DNS
//     重绑定都落在这一层：校验的是解析结果，连的也是那个地址，中间没有被换掉的机会。
//
// 真要在本机内网里抓东西，走 exec_command 那条已经有人确认的路，别从这里开口子。

// ErrBlockedAddr 表示目标解析到了本机、内网或保留地址。调用方用 errors.Is 判它，
// 翻成自己语言的那句话——错误文本本身不给模型看。
var ErrBlockedAddr = errors.New("blocked non-public address")

// BlockedIP 报告这个地址是否不该出站。
//
// IsPrivate 覆盖 10/8、172.16/12、192.168/16 与 fc00::/7；Go 的这几个判断都先看
// IPv4 映射形式，所以 ::ffff:127.0.0.1 也会被挡住。补充的四个是标准库没管的：
// 运营商级 NAT、192.0.0.0/24、基准测试网段与 240/4 保留段。
func BlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 100 && v4[1]&0xc0 == 64: // 100.64.0.0/10
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
		return true
	case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15
		return true
	case v4[0] >= 240: // 240.0.0.0/4
		return true
	}
	return false
}

// BlockedHost 报告 URL 里的主机名是否一定指向本机：localhost（含 .localhost）、
// 或者一个写死的 IP。域名一律放行——解析它要 DNS，而写死的地址才是「一眼就该拦」的
// 那一类，也是模型被一页网页教着去连的那一类。
func BlockedHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return BlockedIP(ip)
	}
	return false
}

// lookupHost 是变量而不是直接调 net.DefaultResolver，只为让测试给出一组假的解析
// 结果：真去查 DNS 的测试既慢又不稳，还得假设某个域名指向内网。
var lookupHost = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, len(addrs))
	for i, a := range addrs {
		out[i] = a.IP
	}
	return out, nil
}

// GuardedTransport 返回一个只连公网地址的 Transport。超时与连接数沿用
// http.DefaultTransport 的那一组，不在这里另立一套。
func GuardedTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           guardedDial(proxyHost()),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// proxyHost 返回环境里配的代理主机，没有则空串。
//
// 拨向代理的那一拨不查地址：本地代理（127.0.0.1:7890 这类）太常见，拦掉的话配了代理
// 的人会突然什么都抓不到。而目标主机由代理去解析，这一层本来就看不见——那是用户自己
// 选的出口，不再由我们判断。
func proxyHost() string {
	req, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		return ""
	}
	u, err := http.ProxyFromEnvironment(req)
	if err != nil || u == nil {
		return ""
	}
	return u.Hostname()
}

// guardedDial 返回一个先查地址再拨号的拨号器。
//
// 注意它拨的是解析出来的那个 IP，不是主机名：如果照主机名再拨一次，解析就会发生两次，
// 第二次的答案可以不一样，检查等于白做。
func guardedDial(proxy string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if proxy != "" && strings.EqualFold(host, proxy) {
			return d.DialContext(ctx, network, addr)
		}
		ips, err := lookupHost(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error = ErrBlockedAddr
		for _, ip := range ips {
			if BlockedIP(ip) {
				continue
			}
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}
