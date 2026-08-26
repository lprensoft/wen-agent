package server

import (
	"embed"
	"io/fs"
	"net/http"

	"wen/internal/agent"
	"wen/internal/modelcfg"
	"wen/internal/plugin"
	"wen/internal/session"
)

//go:embed webui
var webuiFS embed.FS

// Options 是 Server 的访问控制参数。
type Options struct {
	// Auth 保管访问口令；为 nil 表示不设防（仅用于测试）。
	Auth *AuthStore
	// TrustLoopback 决定回环来源是否免认证。套反向代理时必须关掉：
	// 那种部署下所有请求源都是回环地址。
	TrustLoopback bool
	// Exposed 表示实际监听地址覆盖了本机以外的网卡，供界面提示与清除口令的守卫判断。
	Exposed bool
	// Lang 是进程语言，只用在**没有请求可依附**的地方：模型 provider 建出来之后
	// 要为每个人报错，那份文本不能跟着某一次请求走。界面文案仍按 reqLang 判定。
	Lang string
}

type Server struct {
	agent    *agent.Agent
	store    *session.Store
	plugins  *plugin.Manager
	models   *modelcfg.Store
	confirms *confirmBroker
	notices  *noticeHub

	auth          *AuthStore
	tokens        *tokenStore
	gate          *failGate
	trustLoopback bool
	exposed       bool
	lang          string
}

func New(a *agent.Agent, store *session.Store, plugins *plugin.Manager, models *modelcfg.Store, opts Options) *Server {
	auth := opts.Auth
	if auth == nil {
		auth = &AuthStore{} // 无口令 = 不设防
	}
	s := &Server{
		agent: a, store: store, plugins: plugins, models: models,
		confirms: newConfirmBroker(), notices: newNoticeHub(),
		auth: auth, tokens: newTokenStore(), gate: newFailGate(),
		trustLoopback: opts.TrustLoopback, exposed: opts.Exposed, lang: opts.Lang,
	}
	// 接上会话注记的实时出口。后台工作产生的注记本来只能进日志——轮次的事件流
	// 在它跑完之前就关了。
	a.SetNoticeSink(s.notices.publish)
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(webuiFS, "webui")
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("POST /api/sessions", s.createSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	mux.HandleFunc("POST /api/chat", s.chat)
	mux.HandleFunc("POST /api/confirmations/{id}", s.confirmResolve)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/sessions/{id}/compact", s.compact)
	mux.HandleFunc("GET /api/plugins", s.listPlugins)
	mux.HandleFunc("PUT /api/plugins/{name}", s.setPlugin)
	mux.HandleFunc("PUT /api/plugins/{name}/config", s.setPluginConfig)
	mux.HandleFunc("POST /api/plugins/{name}/actions/{key}", s.startPluginAction)
	mux.HandleFunc("GET /api/plugins/{name}/actions/{key}", s.pluginActionState)
	mux.HandleFunc("GET /api/models", s.listModels)
	mux.HandleFunc("PUT /api/models", s.saveModels)
	mux.HandleFunc("PUT /api/models/current", s.setCurrentModel)
	mux.HandleFunc("POST /api/models/test", s.testModel)

	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/login", s.authLogin)
	mux.HandleFunc("POST /api/auth/logout", s.authLogout)
	mux.HandleFunc("PUT /api/auth/password", s.authSetPassword)

	return s.guard(mux)
}
