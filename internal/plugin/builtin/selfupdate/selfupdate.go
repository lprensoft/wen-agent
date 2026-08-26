// Package selfupdate 提供程序自更新：后台按周期查 GitHub 上有没有新的正式版，
// 在设置页上一键下载、校验、替换并重启。
//
// 它是个不给模型任何东西的插件——不注入提示词、不提供工具。做成插件是因为需要的
// 三样东西现成的契约都有：设置页的操作入口（Actionable）、可配置项（Configurable）、
// 状态命令里的一行（StatusReporter），核心因此不必知道「更新」这回事。顺带得到的
// 好处是它可以被关掉，而「关掉自动检查」本来就该是个看得见的开关。
//
// 重启是通过构造函数传进来的一个回调完成的（见 Options.Restart）：怎么重启是启动
// 那一侧的事（Linux 与 macOS 上 execve 换掉进程映像，Windows 上起新进程再退出），
// 插件只负责在替换成功后说一声「可以重启了」。
package selfupdate

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/updater"
	"wen/internal/version"
)

const (
	defaultAutoCheck   = true
	defaultCheckHours  = 24
	defaultAutoRestart = true

	// minCheckHours 是检查周期的下限。GitHub 未鉴权的接口按 IP 限流（每小时 60 次），
	// 而新版本一天也出不了几个，查得再勤也只是白打对方。
	minCheckHours = 1
	maxCheckHours = 720

	// startupGrace 是启动后到第一次检查之间的宽限期。服务刚起来的那一秒还有一堆
	// 插件在初始化，不该在那时候挤一次网络请求进去。
	startupGrace = 2 * time.Minute

	// updateTimeout 是一次完整更新（查询 + 下载 + 校验 + 试运行 + 替换）的总时限。
	updateTimeout = 15 * time.Minute
)

// restartDelay 是「替换完成」到「真的重启」之间的停顿，留给界面把最后一条进展
// 取走——重启一发生，这个进程连同它的 HTTP 服务就没了。是 var 只为测试里调小。
var restartDelay = 1500 * time.Millisecond

// RestartFunc 请求重启服务。由启动那一侧提供（见 Options.Restart）。
// 返回 nil 表示重启流程已经开始，此后这个进程随时可能消失。
type RestartFunc func(reason string) error

// Options 是构造参数。
type Options struct {
	// Restart 用新版程序重启服务；为 nil 表示这个部署形态下重启不可用
	// （插件据此降级为「更新完成，重启后生效」，而不是报错）。
	Restart RestartFunc
	// API、Repo 与 Exe 只在测试里替换（Exe 是「要被替换掉的那个可执行文件」，
	// 不指定就是当前程序自己）。见 updater 包注释：发布仓库不做成配置项。
	API  string
	Repo string
	Exe  string
}

// Plugin 是 self_update 系统插件。
type Plugin struct {
	mu          sync.RWMutex
	lang        string // 进程语言。本插件不给模型任何东西，文案一律走界面语言包
	autoCheck   bool
	interval    time.Duration
	autoRestart bool
	stateDir    string
	exe         string // 当前可执行文件的真实路径
	client      *updater.Client
	restart     RestartFunc
	fixedExe    string // 非空表示可执行文件的位置是外部指定的（测试用）
	st          state
	// lastTry 是上一次尝试检查的时刻（无论成败），只在内存里：一次失败的尝试
	// 不值得写盘，而重启后由启动宽限期兜住（见 check.go 的 retryAfter）。
	lastTry time.Time

	// 后台检查循环。Init 可重入，每次先停旧的再起新的。
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// 设置页操作的状态。StartAction 与 ActionState 会被 HTTP 并发调用，自带一把锁。
	actMu    sync.Mutex
	actSeq   uint64
	actState plugin.ActionState
	running  bool

	// now 可在测试里替换。
	now func() time.Time
}

func New(opts Options) *Plugin {
	c := updater.NewClient("", version.Version)
	if opts.API != "" {
		c.API = opts.API
	}
	if opts.Repo != "" {
		c.Repo = opts.Repo
	}
	return &Plugin{
		fixedExe:    opts.Exe,
		autoCheck:   defaultAutoCheck,
		interval:    defaultCheckHours * time.Hour,
		autoRestart: defaultAutoRestart,
		client:      c,
		restart:     opts.Restart,
		now:         time.Now,
	}
}

func (p *Plugin) Name() string { return "self_update" }

func (p *Plugin) Category() string { return plugin.CategoryProgram }

func (p *Plugin) Description() string {
	return "程序更新：定期检查 GitHub 上的新版本，在设置页一键下载校验、替换并重启"
}

// SystemPrompt 恒为空：更新程序不是模型的事，一个字都不该进它的上下文。
func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) Tools() []plugin.Tool { return nil }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: "auto_check", Label: "自动检查新版本", Type: plugin.FieldBool,
			Description: "后台按下面的周期查一次 GitHub 上有没有新的正式版。只是查，" +
				"不会自己下载或安装——要不要更新始终由你在这里点。",
			Default: defaultAutoCheck,
		},
		{
			Key: "check_hours", Label: "检查周期（小时）", Type: plugin.FieldInt,
			Description: "每隔多久查一次。上次检查的时刻会记下来，重启后接着算，不会每次启动都重查。",
			Default:     defaultCheckHours,
			Min:         plugin.IntPtr(minCheckHours),
			Max:         plugin.IntPtr(maxCheckHours),
		},
		{
			Key: "auto_restart", Label: "更新后自动重启", Type: plugin.FieldBool,
			Description: "替换完程序文件后立刻重启服务，新版立即生效。" +
				"关掉的话更新照做，但要等你下次自己启动才会用上新版。" +
				"注意重启会中断正在进行的对话；远程访问的话重启后需要重新登录。",
			Default: defaultAutoRestart,
		},
	}
}

// Init 应用配置并（重）启动后台检查循环。可重入。
//
// 这里不发网络请求：启动时断网不该让插件启用失败，第一次检查交给后台循环。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.self_update.no_state_dir",
			"没有可用的持久化目录，无法记录上次检查的时刻"))
	}
	exe := p.fixedExe
	if exe == "" {
		resolved, err := updater.ExePath(ictx.Lang)
		if err != nil {
			return err
		}
		exe = resolved
	}

	autoCheck := plugin.CfgBool(cfg, "auto_check", defaultAutoCheck)
	hours := plugin.CfgInt(cfg, "check_hours", defaultCheckHours)
	if hours < minCheckHours {
		hours = minCheckHours
	}
	autoRestart := plugin.CfgBool(cfg, "auto_restart", defaultAutoRestart)

	p.Stop() // 重入：先停上一轮循环，避免两套定时器并行

	p.mu.Lock()
	p.lang = ictx.Lang
	p.client.Lang = ictx.Lang
	p.stateDir = ictx.StateDir
	p.exe = exe
	p.autoCheck = autoCheck
	p.interval = time.Duration(hours) * time.Hour
	p.autoRestart = autoRestart
	p.st = loadState(ictx.StateDir)
	p.mu.Unlock()

	// 上一次更新留下的残留（Windows 上运行中删不掉的旧映像、下载用的工作目录）
	// 只有到下一次启动才轮得到清理
	updater.CleanupOld(exe)
	p.reconcile()

	if !autoCheck {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()
	p.wg.Add(1)
	go p.loop(ctx)
	return nil
}

// Stop 停掉后台循环。禁用、重新配置与进程退出三处都会调用。
//
// 只等循环退出，不等正在进行的更新流程：那一段可能正卡在几十兆的下载上，
// 而它自带超时，最坏情况是替换动作发生在插件已被禁用之后——文件层面仍然是对的。
func (p *Plugin) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		p.wg.Wait()
	}
}

// reconcile 处理「上次更新已经把文件换掉了」这件事在重启之后的收尾。
//
// 判据是当前跑的版本号：与上次记下的目标一致，说明这次启动就是那次更新的结果，
// 于是把设置页上那个还在轮询的进展窗定稿成成功——浏览器那边看到的是同一次操作
// 从「正在重启」直接接到「已完成」，中间那次连接中断由前端的重试兜住。
func (p *Plugin) reconcile() {
	p.mu.Lock()
	pend := p.st.Pending
	if pend == nil {
		p.mu.Unlock()
		return
	}
	p.st.Pending = nil
	msg := ""
	if pend.To == version.Version {
		p.st.LastUpdate = pend.To
		p.st.LastUpdateAt = p.now()
		msg = i18n.Tf(p.lang, "plugin.self_update.notice.done",
			"已更新到 %s（原 %s），服务已重启，新版正在运行。", pend.To, pend.From)
	}
	p.saveLocked()
	p.mu.Unlock()

	if msg != "" {
		p.setAction(plugin.ActionState{Status: plugin.ActionDone, Message: msg})
		log.Printf("self_update: 已更新到 %s（原 %s）", pend.To, pend.From)
		return
	}
	// 文件换过了，跑起来的却还是旧版本：多半是重启没到位，或者程序被从别处启动。
	// 说清楚现状，让人知道该做什么，而不是留一个「更新过但看不出来」的状态。
	warn := i18n.Tf(p.lang, "plugin.self_update.notice.pending",
		"上次已把程序更新到 %s，但当前运行的仍是 %s。重启程序即可用上新版。",
		pend.To, version.Version)
	p.setAction(plugin.ActionState{Status: plugin.ActionError, Message: warn})
	log.Printf("self_update: %s", warn)
}

// snapshot 是一次调用期间使用的配置与状态快照。
type snapshot struct {
	lang        string
	autoCheck   bool
	interval    time.Duration
	autoRestart bool
	stateDir    string
	exe         string
	client      *updater.Client
	restart     RestartFunc
	st          state
}

func (p *Plugin) snapshot() snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return snapshot{
		lang:        p.lang,
		autoCheck:   p.autoCheck,
		interval:    p.interval,
		autoRestart: p.autoRestart,
		stateDir:    p.stateDir,
		exe:         p.exe,
		client:      p.client,
		restart:     p.restart,
		st:          p.st,
	}
}

// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// newerTag 返回「比当前版本新的那个已知版本」，没有则返回空串。
func (s snapshot) newerTag() string {
	if s.st.Latest != "" && updater.Newer(version.Version, s.st.Latest) {
		return s.st.Latest
	}
	return ""
}

var (
	_ plugin.Configurable   = (*Plugin)(nil)
	_ plugin.Categorized    = (*Plugin)(nil)
	_ plugin.Actionable     = (*Plugin)(nil)
	_ plugin.StatusReporter = (*Plugin)(nil)
	_ plugin.Stoppable      = (*Plugin)(nil)
)
