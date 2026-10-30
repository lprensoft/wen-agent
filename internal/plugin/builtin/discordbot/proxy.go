package discordbot

import (
	"fmt"
	"net/http"
	"net/url"

	"wen/internal/i18n"
)

// buildProxy 解析代理配置，并返回 REST 用的 Transport。raw 为空时返回
// (nil, nil)：http.Client 用默认 Transport（会读 HTTPS_PROXY），WebSocket
// 拨号器用 http.ProxyFromEnvironment。
func buildProxy(lang, raw string) (*url.URL, http.RoundTripper, error) {
	if raw == "" {
		return nil, nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf(i18n.T(lang, "plugin.discord_bot.err.proxy_parse",
			"代理地址无法解析: %w"), err)
	}
	switch u.Scheme {
	case "http", "socks5":
	case "":
		return nil, nil, fmt.Errorf("%s", i18n.T(lang, "plugin.discord_bot.err.proxy_scheme",
			"代理地址要带协议前缀，如 socks5://127.0.0.1:1080"))
	default:
		return nil, nil, fmt.Errorf(i18n.T(lang, "plugin.discord_bot.err.proxy_unsupported",
			"不支持的代理协议 %q，Discord 的网关只支持 http、socks5"), u.Scheme)
	}
	if u.Host == "" {
		return nil, nil, fmt.Errorf("%s", i18n.T(lang, "plugin.discord_bot.err.proxy_no_host",
			"代理地址缺少主机和端口"))
	}
	return u, &http.Transport{Proxy: http.ProxyURL(u)}, nil
}
