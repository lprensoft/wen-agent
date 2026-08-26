package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
)

// 访问控制。
//
// 判定按「请求来源」而不是按监听地址：来自回环地址的请求免认证，其余要求会话 Cookie。
// 本机运行因此全程无感（开箱即用不需要设任何口令），远程访问一律要口令，而同一台机器
// 上的 CLI 连 127.0.0.1 也就不必处理任何凭据。
//
// 前面套反向代理时所有请求源都会变成回环地址，那种部署必须把 trust_loopback 关掉，
// 否则认证等于没有。这里刻意不读 X-Forwarded-For：那是个请求头，谁都能伪造，拿它做
// 认证判据比不做认证还危险——不做认证至少还是个看得见的状态。

const (
	// authFileName 是口令文件名，落在配置目录下，与 models.json / plugins.state.json 同级。
	// 口令不进 config.yaml：那个文件永不回写（回写会固化 ${VAR} 展开后的明文并丢注释）。
	authFileName = "auth.json"

	// envPasswordKey 供容器与自动化部署使用。设了它就以它为准，界面上改不动——
	// 两处都能改的时候，「哪一份在生效」就成了要记的规则，而这里没有回写环境变量的办法。
	envPasswordKey = "WEN_AUTH_PASSWORD"

	sessionCookieName = "wen_session"
	sessionTTL        = 7 * 24 * time.Hour

	// pbkdf2Iter 取 OWASP 对 PBKDF2-HMAC-SHA256 的建议值。登录是低频且限速的操作，
	// 单次几百毫秒的代价换的是口令文件泄漏后不好爆破。
	pbkdf2Iter = 600000
	saltLen    = 16
	keyLen     = 32
)

// credential 是 auth.json 的内容。算法名写进文件是为了将来换算法时能认出旧格式。
type credential struct {
	Algo string `json:"algo"`
	Salt string `json:"salt"`
	Hash string `json:"hash"`
	Iter int    `json:"iter"`
}

// AuthStore 保管访问口令。Hash 为空表示尚未设置口令。
type AuthStore struct {
	lang string // 报错说话用的语言，取进程语言
	path string

	mu     sync.RWMutex
	cred   credential
	envPwd string // 来自环境变量，非空时覆盖文件且不可改
}

// NewAuthStore 从 dir/auth.json 加载口令；文件不存在表示未设置。
// 环境变量 WEN_AUTH_PASSWORD 非空时以它为准，不读也不写文件。
func NewAuthStore(lang, dir string) (*AuthStore, error) {
	s := &AuthStore{lang: lang, path: filepath.Join(dir, authFileName)}

	if pwd := strings.TrimSpace(os.Getenv(envPasswordKey)); pwd != "" {
		s.envPwd = pwd
		return s, nil
	}

	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf(i18n.T(s.lang, "server.auth.err.read", "读取 %s: %w"), s.path, err)
	}
	if err := json.Unmarshal(raw, &s.cred); err != nil {
		return nil, fmt.Errorf(i18n.T(s.lang, "server.auth.err.parse", "解析 %s: %w"), s.path, err)
	}
	if s.cred.Hash != "" && s.cred.Algo != "pbkdf2-sha256" {
		return nil, fmt.Errorf(i18n.T(s.lang, "server.auth.err.algo", "%s: 不认识的口令算法 %q"), s.path, s.cred.Algo)
	}
	return s, nil
}

// HasPassword 报告是否已设置访问口令。
func (s *AuthStore) HasPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.envPwd != "" || s.cred.Hash != ""
}

// EnvManaged 报告口令是否来自环境变量（此时界面上不允许修改）。
func (s *AuthStore) EnvManaged() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.envPwd != ""
}

// Verify 校验口令。未设置口令时一律返回 false——「没有口令」不等于「任何口令都对」，
// 免认证由中间件在更外层决定，不在这里混进来。
func (s *AuthStore) Verify(pwd string) bool {
	s.mu.RLock()
	envPwd, cred := s.envPwd, s.cred
	s.mu.RUnlock()

	if envPwd != "" {
		return subtle.ConstantTimeCompare([]byte(envPwd), []byte(pwd)) == 1
	}
	if cred.Hash == "" {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(cred.Salt)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(cred.Hash)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pwd, salt, cred.Iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// 口令变更的几种拒绝理由。CLI 与 HTTP 两个入口共用同一套判定，
// 各自按需要把它们翻译成退出码或状态码。
// 四条口令错误的中文原文单独取常量：哨兵值用它建，处理器按语言取译文时也用它当
// 兜底。覆盖检查是静态扫 (键, 原文) 对的，原文写成 err.Error() 就扫不到——那正是
// 这四条一开始被漏掉的原因。
const (
	MsgEnvManagedZH        = "口令由环境变量 " + envPasswordKey + " 提供，无法在此修改"
	MsgWrongCurrentZH      = "当前口令不正确"
	MsgClearWhileExposedZH = "服务正在对外监听，清除口令会使它完全开放；请先改回只监听本地"
	MsgTooShortZH          = "口令至少 8 位"
)

var (
	ErrEnvManaged        = errors.New(MsgEnvManagedZH)
	ErrWrongCurrent      = errors.New(MsgWrongCurrentZH)
	ErrClearWhileExposed = errors.New(MsgClearWhileExposedZH)
	ErrTooShort          = errors.New(MsgTooShortZH)
)

// minPasswordLen 是口令长度下限。挡的是「1234」这类一试就中的口令，
// 真正的防线是登录限速，所以不必定得更严。
const minPasswordLen = 8

// Change 校验并执行一次口令变更。next 为空表示清除。
//
// 策略集中在这里而不是各自的入口：设置页与 wen config 是两条独立的路径，
// 规则写两份迟早会分叉，而分叉的那一份多半是更宽松的那份。
func (s *AuthStore) Change(current, next string, exposed bool) error {
	if s.EnvManaged() {
		return ErrEnvManaged
	}
	if s.HasPassword() && !s.Verify(current) {
		return ErrWrongCurrent
	}
	if next == "" && exposed {
		return ErrClearWhileExposed
	}
	if next != "" && len([]rune(next)) < minPasswordLen {
		return ErrTooShort
	}
	return s.SetPassword(next)
}

// SetPassword 设置或清除（pwd 为空）访问口令并落盘。
// 调用方一般应当用 Change：它带上了变更策略。
func (s *AuthStore) SetPassword(pwd string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.envPwd != "" {
		return fmt.Errorf(i18n.T(s.lang, "server.auth.env_managed_f", "口令由环境变量 %s 提供，无法在此修改"), envPasswordKey)
	}

	if pwd == "" {
		s.cred = credential{}
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf(i18n.T(s.lang, "server.auth.err.remove", "删除 %s: %w"), s.path, err)
		}
		return nil
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf(i18n.T(s.lang, "server.auth.err.salt", "生成随机盐: %w"), err)
	}
	key, err := pbkdf2.Key(sha256.New, pwd, salt, pbkdf2Iter, keyLen)
	if err != nil {
		return fmt.Errorf(i18n.T(s.lang, "server.auth.err.hash", "计算口令哈希: %w"), err)
	}
	cred := credential{
		Algo: "pbkdf2-sha256",
		Salt: base64.StdEncoding.EncodeToString(salt),
		Hash: base64.StdEncoding.EncodeToString(key),
		Iter: pbkdf2Iter,
	}
	raw, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf(i18n.T(s.lang, "server.auth.err.mkdir", "创建配置目录: %w"), err)
	}
	// 0600：与 models.json、plugins.state.json 一致，文件里是口令派生值
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		return fmt.Errorf(i18n.T(s.lang, "server.auth.err.write", "写入 %s: %w"), s.path, err)
	}
	s.cred = cred
	return nil
}

// ---------- 会话令牌 ----------

// tokenStore 保管已登录的会话令牌。只在内存里：进程重启后需要重新登录，
// 这对一个单人面板是可接受的代价，换来的是不必再落一份可当作凭据用的文件。
type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time // token -> 过期时刻
}

func newTokenStore() *tokenStore {
	return &tokenStore{tokens: map[string]time.Time{}}
}

func (t *tokenStore) issue() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	t.tokens[tok] = time.Now().Add(sessionTTL)
	return tok, nil
}

// valid 校验令牌并顺延有效期（滑动过期：一直在用就不会被踢下线）。
func (t *tokenStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.tokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(t.tokens, tok)
		return false
	}
	t.tokens[tok] = time.Now().Add(sessionTTL)
	return true
}

// revokeAll 让所有已登录会话失效。
func (t *tokenStore) revokeAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.tokens)
}

func (t *tokenStore) revoke(tok string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.tokens, tok)
}

func (t *tokenStore) pruneLocked() {
	now := time.Now()
	for tok, exp := range t.tokens {
		if now.After(exp) {
			delete(t.tokens, tok)
		}
	}
}

// ---------- 登录限速 ----------

// failGate 按来源 IP 限制登录失败次数，挡住在线爆破。
type failGate struct {
	mu      sync.Mutex
	records map[string]*failRecord
}

type failRecord struct {
	count  int
	window time.Time // 本轮计数窗口的结束时刻
}

const (
	maxFailures   = 5
	failureWindow = 5 * time.Minute
)

func newFailGate() *failGate {
	return &failGate{records: map[string]*failRecord{}}
}

// blocked 报告该来源当前是否已被限速。
func (g *failGate) blocked(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.records[ip]
	if !ok {
		return false
	}
	if time.Now().After(r.window) {
		delete(g.records, ip)
		return false
	}
	return r.count >= maxFailures
}

func (g *failGate) fail(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked()
	r, ok := g.records[ip]
	if !ok || time.Now().After(r.window) {
		g.records[ip] = &failRecord{count: 1, window: time.Now().Add(failureWindow)}
		return
	}
	r.count++
	// 每次失败都把窗口往后推：持续尝试的来源会一直被挡着
	r.window = time.Now().Add(failureWindow)
}

func (g *failGate) reset(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.records, ip)
}

func (g *failGate) pruneLocked() {
	now := time.Now()
	for ip, r := range g.records {
		if now.After(r.window) {
			delete(g.records, ip)
		}
	}
}

// ---------- 地址判定 ----------

// IsLoopbackHost 报告监听地址是否只覆盖本机。
// 空串、0.0.0.0、:: 表示所有网卡，按「对外暴露」算。
func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	switch host {
	case "", "0.0.0.0", "::", "[::]", "*":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// remoteIP 取请求的来源 IP。只用 RemoteAddr —— 代理头可以伪造，不能作为认证判据。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopbackRequest(r *http.Request) bool {
	ip := net.ParseIP(remoteIP(r))
	return ip != nil && ip.IsLoopback()
}
