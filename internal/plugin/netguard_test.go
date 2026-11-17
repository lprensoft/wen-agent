package plugin

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3",
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "192.0.0.1",
		"0.0.0.0", "0.1.2.3", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::1", "fe80::1", "fd00::1", "fec0::1", "::", "::ffff:127.0.0.1",
		// NAT64 能把 64:ff9b::/96 里的地址映射回内网 IPv4，a9fe:a9fe 就是 169.254.169.254
		"64:ff9b::a9fe:a9fe",
		"ff02::1",
	}
	for _, s := range blocked {
		if !BlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}

	// 198.18/15 要在放行这一侧：代理的 fake-ip 模式拿它当公网域名的占位地址
	allowed := []string{"8.8.8.8", "1.1.1.1", "172.32.0.1", "198.18.0.1", "198.19.1.1", "2001:4860:4860::8888"}
	for _, s := range allowed {
		if BlockedIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}

	if !BlockedIP(nil) {
		t.Error("an unparsed address must not pass as public")
	}
}

func TestBlockedHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":         true,
		"169.254.169.254":   true,
		"192.168.1.1":       true,
		"::1":               true,
		"localhost":         true,
		"LOCALHOST":         true,
		"localhost.":        true,
		"api.localhost":     true,
		"":                  true,
		"example.com":       false,
		"localhost.evil.co": false,
		"8.8.8.8":           false,
	}
	for host, want := range cases {
		if got := BlockedHost(host); got != want {
			t.Errorf("BlockedHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// stubLookup 把解析固定成一组地址，测试因此不用碰 DNS。
func stubLookup(t *testing.T, ips ...string) {
	t.Helper()
	orig := lookupHost
	lookupHost = func(context.Context, string) ([]net.IP, error) {
		out := make([]net.IP, len(ips))
		for i, s := range ips {
			out[i] = net.ParseIP(s)
		}
		return out, nil
	}
	t.Cleanup(func() { lookupHost = orig })
}

func TestGuardedDialRejectsBlocked(t *testing.T) {
	stubLookup(t, "127.0.0.1")
	// 名字是公网的样子，解析出来却是本机：这是拨号这一层唯一能拦住的那一类
	_, err := guardedDial("")(context.Background(), "tcp", "evil.example:80")
	if !errors.Is(err, ErrBlockedAddr) {
		t.Fatalf("want ErrBlockedAddr, got %v", err)
	}
}

func TestGuardedDialSkipsProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()

	// 本地代理很常见（127.0.0.1:7890 那类）。拨向它的这一拨必须放过去，否则配了
	// 代理的人会突然什么都抓不到。
	conn, err := guardedDial("127.0.0.1")(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("a dial to the configured proxy must be allowed: %v", err)
	}
	_ = conn.Close()
}
