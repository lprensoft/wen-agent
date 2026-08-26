package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newAuthStoreWithPassword(t *testing.T, pwd string) *AuthStore {
	t.Helper()
	s, err := NewAuthStore("", t.TempDir())
	if err != nil {
		t.Fatalf("NewAuthStore: %v", err)
	}
	if pwd != "" {
		if err := s.SetPassword(pwd); err != nil {
			t.Fatalf("SetPassword: %v", err)
		}
	}
	return s
}

func TestAuthStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewAuthStore("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.HasPassword() {
		t.Fatal("新目录不应有口令")
	}
	if err := s.SetPassword("hunter2hunter2"); err != nil {
		t.Fatal(err)
	}

	// 重新加载，确认落盘的哈希能校验
	again, err := NewAuthStore("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !again.HasPassword() {
		t.Fatal("重新加载后应有口令")
	}
	if !again.Verify("hunter2hunter2") {
		t.Error("正确口令未通过校验")
	}
	if again.Verify("hunter2hunter3") {
		t.Error("错误口令通过了校验")
	}

	// 清除后文件应消失，且不再校验通过
	if err := again.SetPassword(""); err != nil {
		t.Fatal(err)
	}
	if again.HasPassword() || again.Verify("hunter2hunter2") {
		t.Error("清除口令后仍在生效")
	}
}

// 未设置口令时 Verify 必须一律为 false：「没有口令」不等于「任何口令都对」。
func TestAuthStoreVerifyWithoutPassword(t *testing.T) {
	s := newAuthStoreWithPassword(t, "")
	if s.Verify("") || s.Verify("whatever") {
		t.Error("未设口令时 Verify 应恒为 false")
	}
}

func TestAuthStoreEnvOverride(t *testing.T) {
	t.Setenv(envPasswordKey, "from-env-password")
	s, err := NewAuthStore("", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasPassword() || !s.EnvManaged() {
		t.Fatal("环境变量应提供口令")
	}
	if !s.Verify("from-env-password") || s.Verify("other") {
		t.Error("环境变量口令校验不正确")
	}
	if err := s.SetPassword("x"); err == nil {
		t.Error("环境变量托管时应拒绝修改口令")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "::1": true, "localhost": true, "127.0.0.2": true,
		"0.0.0.0": false, "::": false, "": false, "192.168.1.10": false,
	}
	for host, want := range cases {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// guard 的核心判定：口令未设=放行；设了则回环免认证、其它来源要 Cookie。
func TestGuardDecisions(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	newSrv := func(pwd string, trustLoopback bool) *Server {
		return &Server{
			auth: newAuthStoreWithPassword(t, pwd), tokens: newTokenStore(),
			gate: newFailGate(), trustLoopback: trustLoopback,
		}
	}

	tests := []struct {
		name          string
		pwd           string
		trustLoopback bool
		remote        string
		path          string
		want          int
	}{
		{"无口令时放行远程", "", true, "203.0.113.5:1234", "/api/sessions", http.StatusOK},
		{"有口令时回环免认证", "hunter2hunter2", true, "127.0.0.1:1234", "/api/sessions", http.StatusOK},
		{"有口令时远程被拦", "hunter2hunter2", true, "203.0.113.5:1234", "/api/sessions", http.StatusUnauthorized},
		{"关掉回环免认证后本机也要登录", "hunter2hunter2", false, "127.0.0.1:1234", "/api/sessions", http.StatusUnauthorized},
		{"登录接口始终放行", "hunter2hunter2", false, "203.0.113.5:1234", "/api/auth/login", http.StatusOK},
		{"页面请求跳登录页", "hunter2hunter2", true, "203.0.113.5:1234", "/", http.StatusFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			r.RemoteAddr = tt.remote
			w := httptest.NewRecorder()
			newSrv(tt.pwd, tt.trustLoopback).guard(ok).ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Errorf("状态码 = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// 登录后拿到的 Cookie 应当能让远程来源通过。
func TestLoginGrantsAccess(t *testing.T) {
	s := &Server{
		auth: newAuthStoreWithPassword(t, "hunter2hunter2"), tokens: newTokenStore(),
		gate: newFailGate(), trustLoopback: true,
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	login := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"password":"hunter2hunter2"}`))
	login.RemoteAddr = "203.0.113.5:1234"
	lw := httptest.NewRecorder()
	s.guard(http.HandlerFunc(s.authLogin)).ServeHTTP(lw, login)
	if lw.Code != http.StatusNoContent {
		t.Fatalf("登录状态码 = %d, body = %s", lw.Code, lw.Body)
	}
	cookies := lw.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("登录未下发 Cookie")
	}

	r := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	r.AddCookie(cookies[0])
	w := httptest.NewRecorder()
	s.guard(ok).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("带 Cookie 的请求状态码 = %d, want 200", w.Code)
	}
}

// 连续失败应触发限速，且限速优先于口令校验（正确口令也要等）。
func TestLoginRateLimit(t *testing.T) {
	s := &Server{
		auth: newAuthStoreWithPassword(t, "hunter2hunter2"), tokens: newTokenStore(),
		gate: newFailGate(), trustLoopback: true,
	}
	attempt := func(pwd string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login",
			strings.NewReader(`{"password":"`+pwd+`"}`))
		r.RemoteAddr = "203.0.113.5:1234"
		w := httptest.NewRecorder()
		s.authLogin(w, r)
		return w.Code
	}
	for i := 0; i < maxFailures; i++ {
		if code := attempt("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败的状态码 = %d, want 401", i+1, code)
		}
	}
	if code := attempt("hunter2hunter2"); code != http.StatusTooManyRequests {
		t.Errorf("限速后状态码 = %d, want 429", code)
	}
}

// /api/auth/status 虽然在放行名单上，但它报告的 authenticated 必须是真实身份，
// 不能因为「这个路径被放行了」就回答已登录。
func TestAuthStatusReportsRealIdentity(t *testing.T) {
	s := &Server{
		auth: newAuthStoreWithPassword(t, "hunter2hunter2"), tokens: newTokenStore(),
		gate: newFailGate(), trustLoopback: false,
	}
	r := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.authStatus(w, r)

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["authenticated"] != false {
		t.Errorf("authenticated = %v, want false", got["authenticated"])
	}
}

// 跨站写请求要被挡住，与是否已认证无关——本机免认证的部署尤其需要这一层。
func TestCrossOriginWriteRejected(t *testing.T) {
	s := &Server{
		auth: newAuthStoreWithPassword(t, ""), tokens: newTokenStore(),
		gate: newFailGate(), trustLoopback: true,
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	r := httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	s.guard(ok).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("跨站写请求状态码 = %d, want 403", w.Code)
	}

	// 同源写请求照常放行
	r2 := httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
	r2.RemoteAddr = "127.0.0.1:1234"
	r2.Header.Set("Origin", "http://"+r2.Host)
	w2 := httptest.NewRecorder()
	s.guard(ok).ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Errorf("同源写请求状态码 = %d, want 200", w2.Code)
	}
}

// 改口令后所有已登录会话失效。
func TestSetPasswordRevokesSessions(t *testing.T) {
	s := &Server{
		auth: newAuthStoreWithPassword(t, "hunter2hunter2"), tokens: newTokenStore(),
		gate: newFailGate(), trustLoopback: true,
	}
	tok, err := s.tokens.issue()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "/api/auth/password",
		strings.NewReader(`{"current":"hunter2hunter2","new":"newpassword1"}`))
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.authSetPassword(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("改口令状态码 = %d, body = %s", w.Code, w.Body)
	}
	if s.tokens.valid(tok) {
		t.Error("改口令后旧会话仍然有效")
	}
	if !s.auth.Verify("newpassword1") {
		t.Error("新口令未生效")
	}
}

// 对外监听时不允许清除口令——那等于把服务完全开放。
func TestClearPasswordBlockedWhenExposed(t *testing.T) {
	s := &Server{
		auth: newAuthStoreWithPassword(t, "hunter2hunter2"), tokens: newTokenStore(),
		gate: newFailGate(), trustLoopback: true, exposed: true,
	}
	r := httptest.NewRequest(http.MethodPut, "/api/auth/password",
		strings.NewReader(`{"current":"hunter2hunter2","new":""}`))
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.authSetPassword(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("状态码 = %d, want 409", w.Code)
	}
	if !s.auth.HasPassword() {
		t.Error("口令被清掉了")
	}
}
