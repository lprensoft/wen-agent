package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"wen/internal/i18n"
)

// freePaths 是不需要认证就能访问的路径：登录页本身、登录接口，以及界面在登录前
// 就要读的状态接口。范围刻意收得很窄——不放开整个静态目录，登录页因此做成自带样式的单文件。
var freePaths = map[string]bool{
	"/login.html":  true,
	"/favicon.ico": true,
	// 登录页上的品牌图标。放行单个文件而不是整个 assets 目录。
	"/assets/logo-512.png": true,
	"/api/auth/status":     true,
	"/api/auth/login":      true,
}

// guard 是访问控制中间件。顺序是先挡跨站请求，再判身份。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 跨站写请求一律拒绝，与是否需要认证无关。本机部署同样需要这一层：
		// 浏览器访问任意网站时，那个页面完全可以向 127.0.0.1:8080 发起写请求，
		// 而回环免认证会把它放行。Cookie 的 SameSite 挡住带凭据的情形，这里挡住其余。
		if !isSafeMethod(r.Method) && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, i18n.T(reqLang(r), "server.err.csrf", "跨站请求被拒绝"))
			return
		}

		if s.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, i18n.T(reqLang(r), "server.err.unauthorized", "未登录"))
			return
		}
		http.Redirect(w, r, "/login.html", http.StatusFound)
	})
}

// authenticated 判定这个请求是否具备访问受保护资源的身份。
// 与 authorized 分开是必要的：freePaths 上的请求会被放行，但那不代表它有身份，
// 而 /api/auth/status 正是要如实回答「我现在算登录了吗」——两者混在一起时，
// 它会因为自己在放行名单上而永远回答「是」，界面据此判断就全错了。
func (s *Server) authenticated(r *http.Request) bool {
	// 未设置口令时不设防。此时启动守卫已保证只监听回环，暴露不了。
	if !s.auth.HasPassword() {
		return true
	}
	// 回环免认证。反代场景下所有请求源都是回环，必须靠 trust_loopback 关掉这条。
	if s.trustLoopback && isLoopbackRequest(r) {
		return true
	}
	c, err := r.Cookie(sessionCookieName)
	return err == nil && s.tokens.valid(c.Value)
}

// authorized 判定这个请求是否放行。
func (s *Server) authorized(r *http.Request) bool {
	return freePaths[r.URL.Path] || s.authenticated(r)
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin 校验 Origin 与本次请求的 Host 一致。
// 没有 Origin 头的按同源处理：浏览器发起的跨站写请求一定带这个头，不带的是 curl、
// CLI 这类客户端，它们本来就不受同源策略约束，拦它们没有意义。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// ---------- 认证接口 ----------

// authStatus 供界面判断该显示登录页还是提示横幅，登录前即可访问。
// 只暴露状态，不暴露口令本身的任何信息。
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"has_password":   s.auth.HasPassword(),
		"env_managed":    s.auth.EnvManaged(),
		"authenticated":  s.authenticated(r),
		"loopback":       isLoopbackRequest(r),
		"trust_loopback": s.trustLoopback,
		"exposed":        s.exposed,
	})
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "server.err.bad_request", "请求格式错误"))
		return
	}

	ip := remoteIP(r)
	if s.gate.blocked(ip) {
		writeError(w, http.StatusTooManyRequests, i18n.T(reqLang(r), "server.err.too_many", "尝试过于频繁，请稍后再试"))
		return
	}
	if !s.auth.Verify(req.Password) {
		s.gate.fail(ip)
		writeError(w, http.StatusUnauthorized, i18n.T(reqLang(r), "server.err.bad_password", "口令不正确"))
		return
	}
	s.gate.reset(ip)

	tok, err := s.tokens.issue()
	if err != nil {
		writeError(w, http.StatusInternalServerError, i18n.T(reqLang(r), "server.err.session_failed", "创建会话失败"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// 不设 Secure：本服务可能跑在纯 HTTP 上（TLS 交给隧道或反向代理）。
		// 设了它在 http:// 下 Cookie 直接不会被保存，登录会表现为「一直失败」。
		MaxAge: int(sessionTTL.Seconds()),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.tokens.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// authSetPassword 设置或清除访问口令。已设过口令时必须提供当前口令——
// 否则任何一个已登录的会话（或本机上的任意进程）都能悄悄把口令换掉。
func (s *Server) authSetPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "server.err.bad_request", "请求格式错误"))
		return
	}
	if err := s.auth.Change(req.Current, req.New, s.exposed); err != nil {
		lang := reqLang(r)
		switch {
		case errors.Is(err, ErrWrongCurrent):
			writeError(w, http.StatusForbidden, i18n.T(lang, "server.auth.wrong_current", MsgWrongCurrentZH))
		case errors.Is(err, ErrEnvManaged):
			writeError(w, http.StatusConflict, i18n.T(lang, "server.auth.env_managed", MsgEnvManagedZH))
		case errors.Is(err, ErrClearWhileExposed):
			writeError(w, http.StatusConflict, i18n.T(lang, "server.auth.clear_exposed", MsgClearWhileExposedZH))
		case errors.Is(err, ErrTooShort):
			writeError(w, http.StatusBadRequest, i18n.T(lang, "server.auth.too_short", MsgTooShortZH))
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// 改动口令后让所有已登录会话失效，包括发起这次修改的那个——
	// 「改口令」的用意通常正是把别人踢下线。
	s.tokens.revokeAll()
	w.WriteHeader(http.StatusNoContent)
}
