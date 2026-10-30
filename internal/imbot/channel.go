package imbot

import (
	"context"
	"strings"
	"sync"
	"time"

	"wen/internal/plugin"
)

const (
	// DefaultConfirmTimeoutSec 是危险操作等待 /apply 或 /deny 的默认时长。
	DefaultConfirmTimeoutSec = 300

	keyConfirmTimeout = "confirm_timeout_sec"
	keyShowThinking   = "show_thinking"
	keyShowTools      = "show_tools"
	keyPushNotices    = "push_notices"
)

// ConfirmTimeoutField 返回「确认等待超时」配置项。各通道的键、默认值与文案只写一份。
func ConfirmTimeoutField(defSec int) plugin.ConfigField {
	if defSec <= 0 {
		defSec = DefaultConfirmTimeoutSec
	}
	return plugin.ConfigField{
		Key: keyConfirmTimeout, Label: "确认等待超时（秒）", Type: plugin.FieldInt,
		Default: defSec, Min: plugin.IntPtr(10), Max: plugin.IntPtr(3600),
		Description: "危险操作发出确认请求后等待 /apply 或 /deny 的时长，超时按拒绝处理",
	}
}

// OutputFields 返回三个与远端输出有关的开关：思考过程、工具名、后台通知。
func OutputFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: keyShowThinking, Label: "展示思考过程", Type: plugin.FieldBool, Default: false,
			Description: "把每轮的思考过程也推送过来（可能很长）",
		},
		{
			Key: keyShowTools, Label: "展示工具调用", Type: plugin.FieldBool, Default: false,
			Description: "把每轮调用了哪些工具也推送过来（只有工具名，不含参数与结果）",
		},
		{
			Key: keyPushNotices, Label: "推送后台通知", Type: plugin.FieldBool, Default: false,
			Description: "把后台工作留下的说明也推送过来（如记忆提炼的记录）。只推共享内容，推送本身不写入会话",
		},
	}
}

// WhitelistField 返回白名单配置项；标签与说明由通道按自己的用户标识写。
func WhitelistField(label, description string) plugin.ConfigField {
	return plugin.ConfigField{
		Key: "whitelist", Label: label, Type: plugin.FieldText, Default: "", Description: description,
	}
}

// APIBaseField 返回 API 地址配置项；默认值与说明由通道给。
func APIBaseField(def, description string) plugin.ConfigField {
	return plugin.ConfigField{
		Key: "api_base", Label: "API 地址", Type: plugin.FieldString, Default: def, Description: description,
	}
}

// ParseWhitelist 把「一行一个」的白名单配置解析成集合。空行忽略，空名单表示拒绝所有人。
func ParseWhitelist(cfg map[string]any) map[string]bool {
	m := map[string]bool{}
	for _, line := range strings.Split(plugin.CfgString(cfg, "whitelist", ""), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			m[s] = true
		}
	}
	return m
}

// ConfirmTimeout 读配置里的确认等待时长。
func ConfirmTimeout(cfg map[string]any) time.Duration {
	return time.Duration(plugin.CfgInt(cfg, keyConfirmTimeout, DefaultConfirmTimeoutSec)) * time.Second
}

// CoreOptions 是各通道构造公共骨架时需要给的全部东西。
// Cfg 里读的是所有通道共用的那几项：确认超时、输出开关与收发节奏。
type CoreOptions struct {
	PluginName string
	Sender     Sender
	Ctx        plugin.InitContext
	Cfg        map[string]any

	Allow       func(userID string) bool
	Push        func(ctx context.Context, userID, text string) bool
	OnAccepted  func(msg Message)
	Typing      func(ctx context.Context, msg Message, active bool)
	Preview     func(ctx context.Context, msg Message, text string)
	ReplyWindow time.Duration
}

// NewCore 组装通道无关的公共骨架。通道只实现 Sender 与自己的协议钩子；
// 白名单、收发节奏、确认超时与输出开关都从配置里读，不再各写一份。
func NewCore(opts CoreOptions) (*Core, error) {
	mergeWindow, humanPace := PaceConfig(opts.Cfg)
	holdWhenBusy, wakeAfter := HoldConfig(opts.Cfg)
	return New(Config{
		PluginName:     opts.PluginName,
		Sender:         opts.Sender,
		Lang:           opts.Ctx.Lang,
		StateDir:       opts.Ctx.StateDir,
		ConfirmTimeout: ConfirmTimeout(opts.Cfg),
		ShowThinking:   plugin.CfgBool(opts.Cfg, keyShowThinking, false),
		ShowTools:      plugin.CfgBool(opts.Cfg, keyShowTools, false),
		PushNotices:    plugin.CfgBool(opts.Cfg, keyPushNotices, false),
		MergeWindow:    mergeWindow,
		HumanPace:      humanPace,
		HoldWhenBusy:   holdWhenBusy,
		WakeAfter:      wakeAfter,
		Allow:          opts.Allow,
		Push:           opts.Push,
		Notice:         opts.Ctx.Notice,
		OnAccepted:     opts.OnAccepted,
		Typing:         opts.Typing,
		Preview:        opts.Preview,
		RunTurn:        opts.Ctx.RunTurn,
		NewSession:     opts.Ctx.NewSession,
		Compact:        opts.Ctx.Compact,
		Status:         opts.Ctx.Status,
		Sessions:       opts.Ctx.Sessions,
		ReplyWindow:    opts.ReplyWindow,
	})
}

// StopChannel 停掉公共骨架并取消通道自己的循环，等它们退出。
// 通道仍需自己加锁取出 cancel/core 并清空字段；这里只统一停机顺序。
func StopChannel(cancel context.CancelFunc, core *Core, wg *sync.WaitGroup) {
	if core != nil {
		core.Stop()
	}
	if cancel != nil {
		cancel()
		if wg != nil {
			wg.Wait()
		}
	}
}
