package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"wen/internal/agent"
	"wen/internal/config"
	"wen/internal/llm"
	"wen/internal/modelcfg"
	"wen/internal/plugin"
	"wen/internal/session"
	"wen/internal/version"
)

// runtime 是一套装配好的运行时：会话存储、插件、模型配置与 Agent。
// serve 与 eval 共用这一份装配——两处各写一份的话，插件的闭包能力（RunTurn、Notice、
// Status……）迟早会在其中一处漏掉一个。
type runtime struct {
	cfg     *config.Config
	workdir string
	store   *session.Store
	plugins *plugin.Manager
	models  *modelcfg.Store
	current modelcfg.Resolved
	agent   *agent.Agent
}

// runtimeOverrides 是装配时可以改写的几处落点。全部为零值时与 serve 完全一致。
type runtimeOverrides struct {
	// SessionDir 非空时会话改存到这里（回放评测用临时目录，不碰真实会话）。
	SessionDir string
	// PluginOpts 透传给插件管理器：改写插件持久化目录、压住某些插件不启用等。
	PluginOpts []plugin.Option
	// Restart 用新版程序重启服务，交给自更新插件在替换完成后调用。
	// 为 nil 表示这个装配形态下重启不可用（评测就是），插件据此降级为
	// 「更新完成，重启后生效」。
	Restart func(reason string) error
}

// buildRuntime 按配置把各层装配起来。
//
// Agent 与插件互相需要：插件在 Agent 之前构造，故各能力都用闭包延迟到实际使用时取值。
// store 是例外，它只依赖配置，因此提前建好直接交给插件——插件的 Init 就要用它
// （心跳启动时要读上次真人交互的时间）。整个进程共用这一个实例：两个 Store 的
// 会话锁互不相识。
func buildRuntime(cfg *config.Config, ov runtimeOverrides) (*runtime, error) {
	workdir := cfg.Agent.Workdir
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	sessionDir := cfg.SessionDir()
	if ov.SessionDir != "" {
		sessionDir = ov.SessionDir
	}

	var (
		ag      *agent.Agent
		models  *modelcfg.Store
		plugins *plugin.Manager
	)
	store, err := session.NewStore(sessionDir)
	if err != nil {
		return nil, fmt.Errorf(tr("cli.err.session_store", "初始化 session 存储失败: %w"), err)
	}

	lang := resolveLang(cfg)
	ictx := plugin.InitContext{
		Workdir:    workdir,
		Lang:       lang,
		Sessions:   store,
		SessionDir: sessionDir,
		Complete: func(ctx context.Context, prompt string) (string, error) {
			if ag == nil {
				return "", fmt.Errorf("%s", tr("cli.err.no_model", "模型尚未就绪"))
			}
			return ag.Complete(ctx, prompt)
		},
		RunTurn: func(ctx context.Context, sessionID, input string) (string, error) {
			if ag == nil {
				return "", fmt.Errorf("%s", tr("cli.err.no_model", "模型尚未就绪"))
			}
			return ag.RunTurn(ctx, sessionID, input)
		},
		Notice: func(ctx context.Context, sessionID, text string) error {
			if ag == nil {
				return fmt.Errorf("%s", tr("cli.err.no_session", "会话尚未就绪"))
			}
			return ag.AppendNotice(ctx, sessionID, text)
		},
		NewSession: func() (string, error) {
			m, err := store.Create()
			return m.ID, err
		},
		Compact: func(ctx context.Context, sessionID string) error {
			if ag == nil {
				return fmt.Errorf("%s", tr("cli.err.no_model", "模型尚未就绪"))
			}
			return ag.CompactTurn(ctx, sessionID)
		},
		// 「那一天有什么可说的」：汇总各启用插件对某一天的贡献（见 plugin.DayReporter），
		// 按日收束的日记靠它在正文前加上天气这类客观事实
		DayFacts: func(date string) []string {
			if plugins == nil {
				return nil
			}
			return plugins.DayFacts(date)
		},
		// 与 server 的 GET /api/status 同源，保证远端界面与 Web UI 的状态输出一致
		Status: func(sessionID string) (plugin.StatusInfo, error) {
			if models == nil {
				return plugin.StatusInfo{}, fmt.Errorf("%s", tr("cli.err.no_status", "状态尚未就绪"))
			}
			provider, model, thinking, contextLength := models.Status()
			info := plugin.StatusInfo{
				Version:  version.Version,
				Provider: provider, Model: model, Thinking: thinking,
				ContextLength: contextLength, MeasuredTokens: -1,
				CompactAt: int(float64(contextLength) * agent.AutoCompactThreshold),
			}
			if plugins != nil {
				info.PluginLines = plugins.StatusLines(lang)
			}
			if sessionID == "" {
				return info, nil
			}
			meta, msgs, err := store.Get(sessionID)
			if err != nil {
				return info, nil // 会话不存在只影响会话部分
			}
			info.HasSession = true
			info.MessageCount = len(msgs)
			info.EstTokens = agent.EstimateStoredTokens(msgs)
			if meta.LastUsage != nil {
				info.MeasuredTokens = meta.LastUsage.PromptTokens + meta.LastUsage.CompletionTokens
				info.CachedTokens = meta.LastUsage.CachedTokens
				info.CacheWriteTokens = meta.LastUsage.CacheWriteTokens
				info.PromptTokens = meta.LastUsage.PromptTokens
			}
			return info, nil
		},
	}
	plugins = buildPlugins(cfg, ictx, ov.Restart, ov.PluginOpts...)

	// 模型配置：config.yaml 提供初始值，界面上的改动存 models.json 并优先生效
	models, err = modelcfg.NewStore(uiLang, filepath.Join(cfg.BaseDir, "models.json"), cfg)
	if err != nil {
		plugins.StopAll()
		return nil, fmt.Errorf(tr("cli.err.load_models", "加载模型配置失败: %w"), err)
	}
	cur, err := models.Resolve(uiLang)
	if err != nil {
		plugins.StopAll()
		return nil, fmt.Errorf(tr("cli.err.bad_models", "模型配置无效: %w"), err)
	}
	provider, err := llm.New(llm.Config{
		Type: cur.Type, BaseURL: cur.BaseURL, APIKey: cur.APIKey, Dialect: cur.Dialect,
		PromptCache: cur.PromptCache,
		Lang:        uiLang,
	})
	if err != nil {
		plugins.StopAll()
		return nil, err
	}

	ag = agent.New(provider, plugins, store, agent.Options{
		Model:         cur.ModelID,
		Temperature:   cur.Temperature,
		MaxTokens:     cur.MaxTokens,
		SystemPrompt:  cfg.Agent.SystemPrompt,
		MaxTurns:      cfg.Agent.MaxTurns,
		Workdir:       workdir,
		Lang:          lang,
		Thinking:      cur.Thinking,
		ContextLength: cur.ContextLength,
	})
	return &runtime{
		cfg: cfg, workdir: workdir, store: store, plugins: plugins,
		models: models, current: cur, agent: ag,
	}, nil
}
