package main

import (
	"fmt"
	"net/url"
	"path/filepath"

	"wen/internal/config"
	"wen/internal/modelcfg"
	"wen/internal/plugin"
	"wen/internal/runlock"
	"wen/internal/server"
)

// backend 是配置工具的数据出口。两种实现：
//
//   - onlineBackend —— 服务正在运行，经它的 HTTP 接口改。改动当场生效，
//     也不会被服务端的全量状态重写抹掉。
//   - offlineBackend —— 服务没在运行，直接读写配置文件。此时无人竞争，安全。
//
// 界面共用同一套表单渲染，只换这一层。
type backend interface {
	// mode 是显示给用户的模式说明。改动什么时候生效必须看得见，不该让人自己判断。
	mode() string

	listPlugins() ([]plugin.Status, error)
	setPluginEnabled(name string, on bool) error
	setPluginConfig(name string, cfg map[string]any) error
	// 插件操作（扫码绑定这类）。只有在线模式可用：操作是运行时行为，
	// 离线模式下插件根本没有初始化，Status 里也不会暴露任何操作。
	startPluginAction(name, key string) error
	pluginActionState(name, key string) (plugin.ActionState, error)

	loadModels() (modelsDoc, error)
	saveModels(modelcfg.File) error
	setCurrentModel(modelcfg.Selection) error

	loadAuth() (authState, error)
	changePassword(current, next string) error
}

// modelsDoc 是模型配置的只读视图。api_key 只有掩码——两种模式都不需要明文：
// 保存时留空即表示沿用旧值。
type modelsDoc struct {
	Providers []providerView     `json:"providers"`
	Current   modelcfg.Selection `json:"current"`
}

type providerView struct {
	Name        string           `json:"name"`
	Type        string           `json:"type"`
	BaseURL     string           `json:"base_url"`
	Dialect     string           `json:"thinking_dialect"`
	PromptCache *bool            `json:"prompt_cache"`
	HasAPIKey   bool             `json:"has_api_key"`
	MaskedKey   string           `json:"api_key_masked"`
	Source      string           `json:"source"`
	Models      []modelcfg.Model `json:"models"`
}

// toFile 把视图还原成可提交的整档配置。api_key 一律留空 = 沿用旧值，
// 由调用方对改过的那一个单独赋值。
func (d modelsDoc) toFile() modelcfg.File {
	providers := make([]modelcfg.Provider, 0, len(d.Providers))
	for _, p := range d.Providers {
		providers = append(providers, modelcfg.Provider{
			Name: p.Name, Type: p.Type, BaseURL: p.BaseURL,
			Dialect: p.Dialect, PromptCache: p.PromptCache, Models: p.Models,
		})
	}
	return modelcfg.File{Version: 1, Providers: providers, Current: d.Current}
}

type authState struct {
	HasPassword bool `json:"has_password"`
	EnvManaged  bool `json:"env_managed"`
	Exposed     bool `json:"exposed"`
	TrustLoop   bool `json:"trust_loopback"`
}

// ---------- 在线 ----------

type onlineBackend struct {
	c    *client
	addr string
}

func (b *onlineBackend) mode() string {
	return trf("cli.mode.online", "在线（服务运行于 %s，改动立即生效）", b.addr)
}

func (b *onlineBackend) listPlugins() ([]plugin.Status, error) {
	var out []plugin.Status
	// 语言随请求带过去：服务端不记任何人的语言，同一个服务可能同时连着
	// 一个中文浏览器和一个英文的 wen config。
	err := b.c.get("/api/plugins?lang="+url.QueryEscape(uiLang), &out)
	return out, err
}

func (b *onlineBackend) setPluginEnabled(name string, on bool) error {
	return b.c.do("PUT", "/api/plugins/"+name, map[string]any{"enabled": on}, nil)
}

func (b *onlineBackend) setPluginConfig(name string, cfg map[string]any) error {
	return b.c.do("PUT", "/api/plugins/"+name+"/config", map[string]any{"config": cfg}, nil)
}

func (b *onlineBackend) startPluginAction(name, key string) error {
	return b.c.do("POST", "/api/plugins/"+name+"/actions/"+key, map[string]any{}, nil)
}

func (b *onlineBackend) pluginActionState(name, key string) (plugin.ActionState, error) {
	var st plugin.ActionState
	err := b.c.get("/api/plugins/"+name+"/actions/"+key, &st)
	return st, err
}

func (b *onlineBackend) loadModels() (modelsDoc, error) {
	var out modelsDoc
	err := b.c.get("/api/models", &out)
	return out, err
}

func (b *onlineBackend) saveModels(f modelcfg.File) error {
	return b.c.do("PUT", "/api/models", f, nil)
}

func (b *onlineBackend) setCurrentModel(sel modelcfg.Selection) error {
	return b.c.do("PUT", "/api/models/current", sel, nil)
}

func (b *onlineBackend) loadAuth() (authState, error) {
	var out authState
	err := b.c.get("/api/auth/status", &out)
	return out, err
}

func (b *onlineBackend) changePassword(current, next string) error {
	return b.c.do("PUT", "/api/auth/password",
		map[string]string{"current": current, "new": next}, nil)
}

// ---------- 离线 ----------

type offlineBackend struct {
	cfg     *config.Config
	plugins *plugin.Manager
	models  *modelcfg.Store
	auth    *server.AuthStore
	exposed bool
}

// newOfflineBackend 在不启动任何插件的前提下装配配置入口。
// 插件用 WithoutInit 注册：读配置项声明、改开关与参数都不需要它们真的跑起来，
// 而照常初始化会把 QQ 的长连接、心跳与定时任务一并拉起来。
func newOfflineBackend(cfg *config.Config) (*offlineBackend, error) {
	models, err := modelcfg.NewStore(uiLang, filepath.Join(cfg.BaseDir, "models.json"), cfg)
	if err != nil {
		return nil, fmt.Errorf(tr("cli.err.load_models", "加载模型配置失败: %w"), err)
	}
	auth, err := server.NewAuthStore(uiLang, cfg.BaseDir)
	if err != nil {
		return nil, fmt.Errorf(tr("cli.err.load_auth", "加载访问口令失败: %w"), err)
	}
	plugins := buildPlugins(cfg, plugin.InitContext{
		Workdir:    cfg.Agent.Workdir,
		SessionDir: cfg.SessionDir(),
	}, nil, plugin.WithoutInit())
	plugins.Resolve()

	return &offlineBackend{
		cfg: cfg, plugins: plugins, models: models, auth: auth,
		exposed: !server.IsLoopbackHost(cfg.Server.Host),
	}, nil
}

func (b *offlineBackend) mode() string {
	return tr("cli.mode.offline", "离线（服务未运行，改动在下次启动时生效）")
}

func (b *offlineBackend) listPlugins() ([]plugin.Status, error) {
	// 离线模式下没有服务端，翻译就在本进程里做——这也是本地化必须是一次
	// 普通的库调用、而不是 HTTP 层某个中间件的原因。
	return plugin.Localize(uiLang, b.plugins.List()), nil
}

func (b *offlineBackend) setPluginEnabled(name string, on bool) error {
	return b.plugins.SetEnabled(name, on)
}

func (b *offlineBackend) setPluginConfig(name string, cfg map[string]any) error {
	return b.plugins.SetConfig(name, cfg)
}

func (b *offlineBackend) startPluginAction(string, string) error {
	return fmt.Errorf("%s", tr("cli.err.action_needs_server", "插件操作需要服务在运行，请先启动 wen"))
}

func (b *offlineBackend) pluginActionState(string, string) (plugin.ActionState, error) {
	return plugin.ActionState{}, fmt.Errorf("%s", tr("cli.err.action_needs_server", "插件操作需要服务在运行，请先启动 wen"))
}

func (b *offlineBackend) loadModels() (modelsDoc, error) {
	view := b.models.Snapshot()
	doc := modelsDoc{Current: view.Current}
	for _, p := range view.Providers {
		models := p.Models
		if models == nil {
			models = []modelcfg.Model{}
		}
		doc.Providers = append(doc.Providers, providerView{
			Name: p.Name, Type: p.Type, BaseURL: p.BaseURL, Dialect: p.Dialect,
			PromptCache: p.PromptCache, HasAPIKey: p.APIKey != "",
			MaskedKey: modelcfg.MaskKey(p.APIKey), Source: p.Source, Models: models,
		})
	}
	return doc, nil
}

func (b *offlineBackend) saveModels(f modelcfg.File) error {
	_, err := b.models.Save(uiLang, f)
	return err
}

func (b *offlineBackend) setCurrentModel(sel modelcfg.Selection) error {
	_, err := b.models.SetCurrent(uiLang, sel)
	return err
}

func (b *offlineBackend) loadAuth() (authState, error) {
	return authState{
		HasPassword: b.auth.HasPassword(),
		EnvManaged:  b.auth.EnvManaged(),
		Exposed:     b.exposed,
		TrustLoop:   b.cfg.Server.TrustLoopbackOrDefault(),
	}, nil
}

func (b *offlineBackend) changePassword(current, next string) error {
	return b.auth.Change(current, next, b.exposed)
}

// openBackend 选择模式：有实例在跑就走在线，否则离线。
func openBackend(cfg *config.Config) (backend, error) {
	if c, ok := dial(cfg.BaseDir); ok {
		addr := c.base
		if info, ok := runlock.Read(cfg.BaseDir); ok {
			addr = info.Addr
		}
		return &onlineBackend{c: c, addr: addr}, nil
	}
	return newOfflineBackend(cfg)
}
