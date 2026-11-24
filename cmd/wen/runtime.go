package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"wen/internal/agent"
	"wen/internal/clock"
	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/llm"
	"wen/internal/modelcfg"
	"wen/internal/plugin"
	"wen/internal/session"
	"wen/internal/usage"
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
	// budget 是本月的预算判定（装配后总是非空：账本不可用时是降级的那一份）；usageDir /
	// usageSettings 是它的两个落点（运行中保存设置要写回同一个文件）；ledger 是它读的
	// 那份账本，服务端 /api/usage 直接复用，不再另开一个。
	budget        *usage.Budget
	ledger        *usage.Ledger
	usageDir      string
	usageSettings string
}

// runtimeOverrides 是装配时可以改写的几处落点。全部为零值时与 serve 完全一致。
type runtimeOverrides struct {
	// SessionDir 非空时会话改存到这里（回放评测用临时目录，不碰真实会话）。
	SessionDir string
	// UsageDir 非空时用量账本落到这里（评测用临时目录，不污染真实账本）。
	UsageDir string
	// PluginOpts 透传给插件管理器：改写插件持久化目录、压住某些插件不启用等。
	PluginOpts []plugin.Option
	// Restart 用新版程序重启服务，交给自更新插件在替换完成后调用。
	// 为 nil 表示这个装配形态下重启不可用（评测就是），插件据此降级为
	// 「更新完成，重启后生效」。
	Restart func(reason string) error
	// Clock 非空时作为进程的「现在几点」（评测用一条可推进的时间线）。
	Clock clock.Clock
	// ManualTime 为真时插件不自起定时循环，后台工作由评测显式驱动。
	// 为真时 Clock 必定非空。
	ManualTime bool
	// BeforeTurn / AfterTurn 包在插件发起的每一轮 RunTurn 外面。评测用它们在
	// 轮次开始前扣预算、结束后记录尝试；生产路径为 nil。
	BeforeTurn func(ctx context.Context) error
	AfterTurn  func(ctx context.Context, err error)
}

// buildRuntime 按配置把各层装配起来。
//
// 装配顺序是「store → 模型配置与 targets → 账本与预算 → InitContext → 插件 → Agent」。
// 预算必须先于插件：buildPlugins 返回时插件循环已经在跑了，而预算若还没建好，
// 这段时间里到期的心跳/定时任务会看到 usageBudget 为 nil 而直接放行——一次绕过
// 预算的模型请求。Agent 只能排在插件之后（它需要 Manager），于是插件的各能力都用
// 闭包延迟到实际使用时取值，在 ag 发布之前一律返回 plugin.ErrTurnNotStarted。
//
// store 只依赖配置，提前建好直接交给插件——插件的 Init 就要用它（心跳启动时要读
// 上次真人交互的时间）。整个进程共用这一个实例：两个 Store 的会话锁互不相识。
func buildRuntime(cfg *config.Config, ov runtimeOverrides) (*runtime, error) {
	workdir := cfg.Agent.Workdir
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	sessionDir := cfg.SessionDir()
	if ov.SessionDir != "" {
		sessionDir = ov.SessionDir
	}
	now := time.Now
	if ov.Clock != nil {
		now = ov.Clock.Now
	}

	// runTurn 把评测的预算钩子包在插件发起的每一轮外面。生产路径两个钩子都为 nil，
	// 只是一层直接调用。
	runTurn := func(ctx context.Context, run func() (string, error)) (string, error) {
		if ov.BeforeTurn != nil {
			if err := ov.BeforeTurn(ctx); err != nil {
				return "", err
			}
		}
		out, err := run()
		if ov.AfterTurn != nil {
			ov.AfterTurn(ctx, err)
		}
		return out, err
	}

	var (
		// ag 是插件的各能力闭包读取的 Agent。插件循环在 buildPlugins 返回时就已经
		// 在跑，而 Agent 只能等装配走完才发布——两者之间因此是真正的并发：用原子
		// 指针发布，读侧 ag.Load() 要么看到 nil（一律返回 ErrTurnNotStarted），
		// 要么看到完全配置好的那一个，不会读到半截。
		ag     atomic.Pointer[agent.Agent]
		models *modelcfg.Store
		// plugins 与 ag 同理：插件循环在 buildPlugins 返回前就已经在跑，启动期的 IM
		// /status 与心跳整理会与最后那一次赋值并发。用原子指针发布，读侧一律 Load()，
		// nil 就按「插件还没就绪」降级。
		plugins atomic.Pointer[plugin.Manager]
		// budget 在本函数前段装配（要等账本）。闭包晚于它创建，读到的就是最终值。
		budget *usage.Budget
	)
	store, err := session.NewStore(sessionDir)
	if err != nil {
		return nil, fmt.Errorf(tr("cli.err.session_store", "初始化 session 存储失败: %w"), err)
	}

	lang := resolveLang(cfg)
	// 模型配置：config.yaml 提供初始值，界面上的改动存 models.json 并优先生效。
	// 它必须排在插件之前：预算要用它的价格表，而预算又要先于插件。
	models, err = modelcfg.NewStore(uiLang, filepath.Join(cfg.BaseDir, "models.json"), cfg)
	if err != nil {
		return nil, fmt.Errorf(tr("cli.err.load_models", "加载模型配置失败: %w"), err)
	}
	chain, err := models.ResolveChain(uiLang)
	if err != nil {
		return nil, fmt.Errorf(tr("cli.err.bad_models", "模型配置无效: %w"), err)
	}
	cur := chain[0]
	targets := make([]agent.ModelTarget, 0, len(chain))
	for _, r := range chain {
		provider, err := llm.New(r.LLMConfig(uiLang))
		if err != nil {
			return nil, err
		}
		targets = append(targets, agent.ModelTarget{
			Provider: provider,
			Options: agent.ModelOptions{
				Model:         r.ModelID,
				Temperature:   r.Temperature,
				MaxTokens:     r.MaxTokens,
				Thinking:      r.Thinking,
				ContextLength: r.ContextLength,
			},
			Label:        r.ProviderName + " / " + r.ModelID,
			ProviderName: r.ProviderName,
		})
	}

	usageDir := usageDirFor(cfg, ov)
	usageSettings := usageSettingsFor(cfg, ov)
	// 预算是配置出来的事实，账本可用与否只是它可不可靠：两者分开处置，否则一次探活
	// 失败会让「限额已配置」从状态里整个消失，用户看不出自己的上限为什么没生效。
	ledger, writable := attachUsageLedger(lang, usageDir)
	// 价格表来自模型配置：金额与 token 都由它折算，装配时把同一个查价函数交给预算。
	budget = newUsageBudget(ledger, writable, usageSettings, lang, now, models.Price)

	embed, embedModel := embedFuncs(lang, func() *modelcfg.Store { return models })
	ictx := plugin.InitContext{
		Workdir:    workdir,
		Lang:       lang,
		Sessions:   store,
		SessionDir: sessionDir,
		Clock:      ov.Clock,
		ManualTime: ov.ManualTime,
		Embed:      embed,
		EmbedModel: embedModel,
		Complete: func(ctx context.Context, prompt string) (string, error) {
			a, err := pluginAgent(ag.Load(), tr("cli.err.no_model", "模型尚未就绪"))
			if err != nil {
				return "", err
			}
			return a.Complete(ctx, prompt)
		},
		RunTurn: func(ctx context.Context, sessionID, input string) (string, error) {
			a, err := pluginAgent(ag.Load(), tr("cli.err.no_model", "模型尚未就绪"))
			if err != nil {
				return "", err
			}
			return runTurn(ctx, func() (string, error) { return a.RunTurn(ctx, sessionID, input) })
		},
		RunTurnImages: func(ctx context.Context, sessionID, input string, images []llm.ImagePart) (string, error) {
			a, err := pluginAgent(ag.Load(), tr("cli.err.no_model", "模型尚未就绪"))
			if err != nil {
				return "", err
			}
			return runTurn(ctx, func() (string, error) { return a.RunTurnWithImages(ctx, sessionID, input, images) })
		},
		RunTurnMedia: func(ctx context.Context, sessionID, input string, images []llm.ImagePart, files []llm.FilePart) (string, error) {
			a, err := pluginAgent(ag.Load(), tr("cli.err.no_model", "模型尚未就绪"))
			if err != nil {
				return "", err
			}
			return runTurn(ctx, func() (string, error) { return a.RunTurnWithMedia(ctx, sessionID, input, images, files) })
		},
		Notice: func(ctx context.Context, sessionID, text string) error {
			a, err := pluginAgent(ag.Load(), tr("cli.err.no_session", "会话尚未就绪"))
			if err != nil {
				return err
			}
			return a.AppendNotice(ctx, sessionID, text)
		},
		NewSession: func() (string, error) {
			m, err := store.CreateAt(now())
			return m.ID, err
		},
		Compact: func(ctx context.Context, sessionID string) error {
			a, err := pluginAgent(ag.Load(), tr("cli.err.no_model", "模型尚未就绪"))
			if err != nil {
				return err
			}
			return a.CompactTurn(ctx, sessionID)
		},
		// 「那一天有什么可说的」：汇总各启用插件对某一天的贡献（见 plugin.DayReporter），
		// 按日收束的日记靠它在正文前加上天气这类客观事实
		DayFacts: func(date string) []string {
			mgr := plugins.Load()
			if mgr == nil {
				return nil
			}
			return mgr.DayFacts(date)
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
			if mgr := plugins.Load(); mgr != nil {
				info.PluginLines = mgr.StatusLines(lang)
			}
			// 预算与会话无关：没有 sessionID 也照常填，远端状态想在哪儿展示都行。
			// 预算先于插件建好，插件一起来就能从状态里看到它。
			if budget != nil {
				b := budgetInfo(budget.Status(now()))
				info.Budget = &b
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
	// 走到这里账本与预算都已经装好：buildPlugins 返回时插件的定时循环已经在跑，
	// 它们看到的就是上面那一份 budget，不会再出现「预算还没建好」的窗口。
	//
	// 先存进局部 mgr 再发布：发布之后没有裸读，读侧要么看到 nil（插件还没就绪，
	// 相关能力降级），要么看到完全装配好的那一个。
	mgr := buildPlugins(cfg, ictx, ov.Restart, ov.PluginOpts...)
	plugins.Store(mgr)

	// Agent 只能排在插件之后（它需要 Manager）。先配置好放进局部变量，最后一步才
	// 发布到 ag：在那之前插件闭包看到的 ag 仍是 nil，一律返回 ErrTurnNotStarted。
	configured := agent.New(targets[0].Provider, mgr, store, agent.Options{
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
	configured.SetModelTargets(targets)
	if ledger != nil {
		configured.SetUsageLedger(ledger)
	}
	configured.SetUsageBudget(budget)
	// 发布：到这一步 configured 已经带齐 targets、账本与预算，插件的闭包从此读到
	// 的就是它。先配置后发布是「内容完整」的保证，原子指针是「读得到完整内容」的
	// 保证——普通赋值在这两者之间还有一处数据竞争。
	ag.Store(configured)
	return &runtime{
		cfg: cfg, workdir: workdir, store: store, plugins: mgr,
		models: models, current: cur, agent: configured,
		budget: budget, ledger: ledger, usageDir: usageDir, usageSettings: usageSettings,
	}, nil
}

// pluginAgent 取插件闭包该用的 Agent。装配顺序是「账本与预算 → 插件 → Agent」：
// 插件循环在 buildPlugins 返回时就已经跑起来，而 ag 要等装配走完才发布。这段窗口
// 里到期的后台工作必须停在门口——放过去就是一次绕过预算的模型请求。
//
// ready 是给用户看的那句说明（调用点先翻好再传进来）。返回的错误必须可识别为
// plugin.ErrTurnNotStarted：心跳与调度器据此回滚待办、把任务留到下一次，而不是
// 记成一次失败；同时因为没往下走，不会有任何模型调用。
//
// 调用点一律传 ag.Load()：插件 goroutine 与启动流程之间只有这一个交接点，读完
// 这一行之后的判断用的是同一份快照。
func pluginAgent(ag *agent.Agent, ready string) (*agent.Agent, error) {
	if ag == nil {
		return nil, fmt.Errorf("%w: %s", plugin.ErrTurnNotStarted, ready)
	}
	return ag, nil
}

// usageDirFor 返回用量账本的目录。评测把整场落盘都搬进临时目录，真实账本不能
// 被评测的调用污染。
func usageDirFor(cfg *config.Config, ov runtimeOverrides) string {
	if ov.UsageDir != "" {
		return ov.UsageDir
	}
	return filepath.Join(cfg.BaseDir, "usage")
}

// usageSettingsFor 返回预算设置（usage.json）的路径。
//
// 评测把账本落点改到临时目录时，设置也跟着放在它的旁边：那通常是一份不存在的
// 文件，于是评测读到默认值（不设限），不会被开发机上真实设置的限额拦住。
func usageSettingsFor(cfg *config.Config, ov runtimeOverrides) string {
	if ov.UsageDir != "" {
		return filepath.Join(filepath.Dir(ov.UsageDir), "usage.json")
	}
	return filepath.Join(cfg.BaseDir, "usage.json")
}

// attachUsageLedger 打开账本并探一次写入，返回账本与它是否真的可写。
//
// 失败只记日志、继续启动：用量统计是旁路，不该成为服务起不来的理由。
//
// 打开之后再探一次写入。Open 是惰性的，目录不可写它照样成功；不探的话，那件事
// 要等到第一次记账才暴露，而服务里那等于每个请求都失败、各刷一条日志。
//
// Probe 失败仍然把账本交出去：目录修好之后 Append 自己就能恢复，而记账的失败
// 只提示一次（Agent.usageWarned）。第二个返回值因此表达的是「现在可不可靠」，
// 由调用方拿去把预算标成降级，而不是「要不要接上」。
//
// 它不碰 Agent：账本要在 Agent 建好之前就绪（预算依赖它），装配的最后一步才把
// 返回的账本交给 Agent。
func attachUsageLedger(lang, dir string) (*usage.Ledger, bool) {
	ledger, err := usage.Open(dir)
	if err != nil {
		i18n.Logf(lang, "log.usage.open_failed",
			"usage: 打开用量账本失败（本次运行不记账）: %v", err)
		return nil, false
	}
	if err := ledger.Probe(); err != nil {
		i18n.Logf(lang, "log.usage.probe_failed",
			"usage: 用量账本目录不可写（本次运行不记账）: %v", err)
		return ledger, false
	}
	return ledger, true
}

// newUsageBudget 读设置并建立预算判定。
//
// 预算本身照建：设置读不出来时按默认值（不设限）起、账本不可用时按降级起，两种情况
// 在状态里都看得出「这份判定不可靠」，而不是让预算字段整个消失——预算失效不该把
// 用户关在自己的程序外面。两者都只是起点：账本侧在一次成功的重扫或记账后自愈，
// 设置侧要调用方确认（/api/usage 读得到设置、或保存过一次设置）后清掉。
//
// lookup 是查价用的价格表（通常是 models.Price）。为 nil 表示这次装配没有价格可用：
// token 上限照常，金额上限不生效（状态里由 CostReady=false 交代）。
func newUsageBudget(ledger *usage.Ledger, writable bool, settingsPath, lang string, now func() time.Time, lookup usage.PriceLookup) *usage.Budget {
	settings, err := usage.LoadSettings(settingsPath)
	b := usage.NewBudget(settings, ledger, lookup, now())
	if err != nil {
		// 先建预算再标记降级：降级标记不参与载入，放哪儿都一样，但这样日志里的
		// 错误与已经生效的判定是对得上的。
		b.MarkDegraded()
		i18n.Logf(lang, "log.usage.settings_failed",
			"usage: 读取用量预算设置失败（本次运行按不设限处理）: %v", err)
		return b
	}
	if ledger == nil || !writable {
		// 账本读不出来时已用数恒为 0，按它判定只会得出「还绰绰有余」这种假话。
		// 探活失败（目录写不进去）单独标，一次成功的重扫或记账就能恢复（见 Budget）。
		b.MarkLedgerDegraded()
	}
	return b
}

// budgetInfo 把预算快照转成插件侧的状态形状。
//
// usage.BudgetStatus 与 plugin.BudgetInfo 逐字段对应，但 plugin 故意不认识 usage
// （见 BudgetInfo 的说明），于是这份搬运只能写出来。抽成一个函数是为了它只有一处：
// 漏搬一个字段不会编译失败，只会在某一处输出里静默消失（第 5 步加金额字段时
// 最容易踩）。测试逐字段盯着它。
func budgetInfo(st usage.BudgetStatus) plugin.BudgetInfo {
	return plugin.BudgetInfo{
		Enabled:        st.Enabled,
		Warn:           st.Warn,
		Exceeded:       st.Exceeded,
		Degraded:       st.Degraded,
		LimitTokens:    st.LimitTokens,
		SpentTokens:    st.SpentTokens,
		LimitCost:      st.LimitCost,
		SpentCost:      st.SpentCost,
		Currency:       st.Currency,
		CostReady:      st.CostReady,
		CostInexact:    st.CostInexact,
		UnpricedTokens: st.UnpricedTokens,
		CostUnpriced:   st.CostUnpriced,
		ForeignCost:    st.ForeignCost,
		WarnPercent:    st.WarnPercent,
		Over:           st.Over,
		Month:          st.Month,
		SkippedFiles:   st.SkippedFiles,
		WriteFailed:    st.WriteFailed,
	}
}
