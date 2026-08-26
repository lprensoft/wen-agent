// Package scheduler 让模型在对话中创建定时任务，由后台在到点时以任务内容跑一轮对话。
//
// 任务执行永远不在调度 goroutine 里进行：每个到期任务单独开 goroutine，带 recover 与
// 超时，单个任务卡死或崩溃不影响主程序与其它任务。输出落到最近活跃的会话
// （与心跳插件相同的判定：按真人交互时间，后台轮次不算）。
package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 常量集中在此，便于对照行为。
const (
	taskPrefixZH   = "【定时任务】" // 任务内容作为轮次输入时的前缀，让模型知道这不是用户现场消息
	runTimeout     = 15 * time.Minute
	busyRetryEvery = time.Minute // once 任务撞上会话忙时的重试间隔
	busyRetryMax   = 5
	doneKeep       = 24 * time.Hour // 已完成任务保留时长，供 list_tasks 查看
	missedOnceMax  = 24 * time.Hour // 宕机期间错过的 once 任务：不超过该时长则补跑，否则标记过期
	pollFallback   = 30 * time.Second
	maxTasks       = 200 // 防失控上限：超过后拒绝新建
)

// Plugin 是定时任务插件。有状态：Init 可被反复调用，字段由 mu 保护。
type Plugin struct {
	mu sync.Mutex

	lang       string // 进程语言，注入片段与工具文本据此取用
	stateDir   string
	runTurn    plugin.RunTurnFunc
	newSession plugin.NewSessionFunc
	sessions   plugin.SessionQuery // 只读：挑选最近活跃会话

	tasks []*Task

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Name() string { return "scheduler" }

func (p *Plugin) Category() string { return plugin.CategoryBackground }

func (p *Plugin) Description() string {
	return "对话中创建定时任务（一次性/周期/每日），到点后台执行并把结果写进最近活跃的会话"
}

// guideZH 是定时任务的用法判据。
const guideZH = "你可以管理定时任务：用 schedule_task 创建（支持一次性、按分钟周期、每日定点三种），" +
	"用 list_tasks 查看，用 cancel_task 取消。用户提出“稍后/定时/每天提醒或执行某事”时应使用这些工具。" +
	"对方随口提到将来的事（约好的时间、要办的事、值得关心的时点）时，即使没有明说要提醒，" +
	"也可以顺手定一个一次性任务，到时候主动提起。" +
	"这些任务是对对方的提醒与承诺，到点由你执行；你自己一天里要去做的事（出门、赴约、工作）不是定时任务，归日程工具。"

func (p *Plugin) SystemPrompt() string {
	return prompt.T(p.langOf(), "prompt.scheduler.guide", guideZH)
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{
		&scheduleTool{p: p},
		&listTool{p: p},
		&cancelTool{p: p},
	}
}

// Init 加载任务并（重）启动调度循环。可重入：先停上一轮。
// langOf 取进程语言。
func (p *Plugin) langOf() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lang
}

func (p *Plugin) Init(ictx plugin.InitContext, _ map[string]any) error {
	if ictx.StateDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.scheduler.no_state_dir",
			"没有可用的持久化目录，无法保存任务"))
	}
	if ictx.RunTurn == nil {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.scheduler.no_run_turn",
			"当前环境不支持插件发起对话轮次"))
	}
	if ictx.Sessions == nil {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.scheduler.no_sessions",
			"当前环境不支持会话查询"))
	}

	p.Stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lang = ictx.Lang
	p.stateDir = ictx.StateDir
	p.runTurn = ictx.RunTurn
	p.newSession = ictx.NewSession
	p.sessions = ictx.Sessions
	if err := p.loadLocked(); err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.scheduler.err.load", "加载任务失败: %w"), err)
	}
	p.catchUpLocked()

	ctx, cancel := context.WithCancel(context.Background())
	p.ctx, p.cancel = ctx, cancel
	p.wake = make(chan struct{}, 1)
	p.wg.Add(1)
	go p.loop(ctx)
	return nil
}

// Stop 停止调度循环。执行中的任务 goroutine 通过同一个 ctx 收到取消。
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

// wakeup 通知调度循环任务表变了（新建/取消），重算下一次到期时间。
func (p *Plugin) wakeup() {
	p.mu.Lock()
	wake := p.wake
	p.mu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}
