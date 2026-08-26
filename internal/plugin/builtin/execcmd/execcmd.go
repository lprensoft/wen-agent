// Package execcmd 提供执行 shell 命令的系统插件。
package execcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	outputMaxBytes        = 32 * 1024
	defaultTimeoutSeconds = 60
	defaultConfirmTimeout = 300
	confirmDetailMaxRunes = 4000 // 命令原文进确认卡片，长到异常时截断避免撑爆界面
)

// Plugin 是 exec_command 系统插件。
type Plugin struct {
	mu sync.RWMutex

	workdir        string
	lang           string
	timeout        time.Duration
	guard          string
	confirmTimeout time.Duration
}

func New() *Plugin {
	return &Plugin{
		timeout:        defaultTimeoutSeconds * time.Second,
		guard:          defaultGuard,
		confirmTimeout: defaultConfirmTimeout * time.Second,
	}
}

func (p *Plugin) Name() string { return "exec_command" }

func (p *Plugin) Category() string { return plugin.CategoryTools }
func (p *Plugin) Description() string {
	return "在工作目录下执行 shell 命令，危险操作先由用户确认"
}

// SystemPrompt 在拦截开启时告诉模型这条规则。不说明的话，被拒绝的模型会去改写命令
// 重试——那正是最该避免的行为。
func (p *Plugin) SystemPrompt() string {
	if p.snapshot().guard == guardOff {
		return ""
	}
	return prompt.T(p.snapshot().lang, "prompt.exec_command.guard", promptGuardZH)
}

const promptGuardZH = `[命令执行]
可能造成不可逆后果的命令（删除、覆盖、改写提交历史、改动系统配置等）会先交由对方确认，
个别极高危的命令直接被拒绝。被拒绝或未获同意时，说明原因并另找办法，不要改写命令绕过、
也不要反复重试。破坏性操作请一条一条单独执行，不要串联在一条命令里——那样对方无法只同意
其中安全的部分。`

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key:   "guard",
			Label: "危险命令拦截",
			Type:  plugin.FieldSelect,
			Description: "危险：删除、覆盖、改写历史、改动系统配置等操作先由你确认，其余直接执行（推荐）。" +
				"全部：除只读命令外一律确认。关闭：不拦截。" +
				"注意这是按命令文本判断的，挡的是误操作，挡不住刻意混淆——只有「全部」这一档是完整的。",
			Default: defaultGuard,
			Options: []plugin.ConfigOption{
				{Value: guardDangerous, Label: "危险命令需确认"},
				{Value: guardAll, Label: "全部命令需确认（只读除外）"},
				{Value: guardOff, Label: "关闭"},
			},
		},
		{
			Key:         "timeout_seconds",
			Label:       "命令超时（秒）",
			Type:        plugin.FieldInt,
			Description: "单条命令的最长执行时间，超时后终止并把已有输出返回给模型。",
			Default:     defaultTimeoutSeconds,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(3600),
		},
		{
			Key:         "confirm_timeout_seconds",
			Label:       "等待确认超时（秒）",
			Type:        plugin.FieldInt,
			Description: "超过这个时间没有回应就按拒绝处理。",
			Default:     defaultConfirmTimeout,
			Min:         plugin.IntPtr(10),
			Max:         plugin.IntPtr(3600),
		},
	}
}

func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workdir = ictx.Workdir
	p.lang = ictx.Lang
	p.timeout = time.Duration(plugin.CfgInt(cfg, "timeout_seconds", defaultTimeoutSeconds)) * time.Second
	p.guard = plugin.CfgString(cfg, "guard", defaultGuard)
	p.confirmTimeout = time.Duration(plugin.CfgInt(cfg, "confirm_timeout_seconds", defaultConfirmTimeout)) * time.Second
	return nil
}

// settings 是一次调用期间使用的配置快照（SetConfig 会在运行时重新 Init）。
type settings struct {
	workdir        string
	lang           string
	timeout        time.Duration
	guard          string
	confirmTimeout time.Duration
}

func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		workdir:        p.workdir,
		lang:           p.lang,
		timeout:        p.timeout,
		guard:          p.guard,
		confirmTimeout: p.confirmTimeout,
	}
}

func (p *Plugin) Tools() []plugin.Tool { return []plugin.Tool{&tool{p: p}} }

type tool struct {
	p *Plugin
}

func (t *tool) Name() string { return "exec_command" }

func (t *tool) Description() string {
	return prompt.T(t.p.snapshot().lang, "prompt.exec_command.tool.exec_command.desc",
		"在工作目录下执行一条 shell 命令并返回其输出（stdout 与 stderr 合并）。使用的 shell 见系统环境信息。"+
			"有超时限制，输出过长会被截断。可能造成不可逆后果的命令会先交由对方确认，个别极高危的命令直接拒绝。")
}

// schemaZH 是参数 Schema。整段一个键：它是手写的 JSON，拆成一个字段一个键的话，
// 译者要照着结构去拼，而拼错一个引号就是一份非法的 Schema。
const schemaZH = `{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "要执行的 shell 命令"}
		},
		"required": ["command"]
	}`

func (t *tool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.snapshot().lang, "prompt.exec_command.tool.exec_command.schema", schemaZH))
}

func (t *tool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Command == "" {
		return "", fmt.Errorf("command is required")
	}

	s := t.p.snapshot()
	if err := t.gate(ctx, s, a.Command); err != nil {
		return "", err
	}

	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", a.Command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", a.Command)
	}
	cmd.Dir = s.workdir

	out, err := cmd.CombinedOutput()
	out = plugin.NormalizeEncoding(out)
	out, truncated := plugin.TruncateUTF8(out, outputMaxBytes)

	result := string(out)
	if truncated {
		result += prompt.Tf(s.lang, "prompt.exec_command.result.truncated",
			"\n...[输出超过 %d 字节，已截断]", outputMaxBytes)
	}
	if ctx.Err() == context.DeadlineExceeded {
		result += prompt.Tf(s.lang, "prompt.exec_command.result.timeout",
			"\n[命令超时（%s），已终止]", timeout)
		return result, nil
	}
	if err != nil {
		// 命令非零退出也把输出带回给模型，方便其自行判断
		result += prompt.Tf(s.lang, "prompt.exec_command.result.exit_error",
			"\n[命令退出异常: %v]", err)
	}
	return result, nil
}
