package imbot

import (
	"context"
	"errors"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/statustext"
)

// helpTextZH 是各通道共用的命令说明。命令集统一，换一条通道不用重新学。
// 命令本身（/new 这些）不翻译——它们是要照着打进去的字面量。
const helpTextZH = `支持的命令：
/new     新建一个会话（之后的消息进入新会话）
/status  查看当前会话状态
/compact 压缩当前会话历史
/help    显示本列表
/apply   允许等待中的危险操作
/deny    拒绝等待中的危险操作
其他内容将作为消息发给助手。`

// HelpText 按语言给出命令说明。
func HelpText(lang string) string {
	return i18n.T(lang, "imbot.help", helpTextZH)
}

// process 处理一条已排到队首的消息：命令直接答复，其余作为一轮对话执行。
func (c *Core) process(ctx context.Context, msg Message) {
	defer func() {
		if r := recover(); r != nil {
			i18n.Logf(c.cfg.Lang, "log.imbot.handle_panic", "%s: 处理消息时 panic（已隔离）: %v", c.cfg.PluginName, r)
		}
	}()

	text := strings.TrimSpace(msg.Text)
	switch {
	case text == "/help":
		c.send(ctx, msg, HelpText(c.cfg.Lang))
	case text == "/new":
		c.cmdNew(ctx, msg)
	case text == "/status":
		c.cmdStatus(ctx, msg)
	case text == "/compact":
		c.cmdCompact(ctx, msg)
	case strings.HasPrefix(text, "/"):
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.unknown_command", "未知命令：%s\n%s",
			text, HelpText(c.cfg.Lang)))
	default:
		c.chat(ctx, msg)
	}
}

// chat 把消息作为一轮对话执行，回复助手的最终文本。
func (c *Core) chat(ctx context.Context, msg Message) {
	sid, err := c.SessionFor(msg.UserID)
	if err != nil {
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.err.session_init", "会话初始化失败：%s", err.Error()))
		return
	}

	tctx, cancel := context.WithTimeout(ctx, c.cfg.TurnTimeout)
	defer cancel()
	// 对面是真人：标记交互（更新会话活跃时间），并接入本通道的确认通道
	tctx = plugin.WithInteractive(tctx)
	tctx = plugin.WithConfirmer(tctx, c.confirmerFor(msg))
	tctx = withHeld(tctx, msg) // 被扣过的话，让这一轮知道自己回晚了
	// 按配置转发过程通知：思考链、工具调用（仅名字）。同步发送保证与最终回复的顺序
	if c.cfg.ShowThinking || c.cfg.ShowTools {
		// 过程通知与最终回复走同一条出口：装了分通道路由时，思考链跟着回复一起
		// 转投，否则某一侧人格的思考过程会漏在另一侧的窗口里
		tctx = plugin.WithTurnNotes(tctx, func(n plugin.TurnNote) {
			switch {
			case n.Kind == plugin.NoteThinking && c.cfg.ShowThinking:
				c.reply(ctx, msg, sid, ThinkingLine(c.cfg.Lang, n.Text))
			case n.Kind == plugin.NoteToolCalls && c.cfg.ShowTools:
				c.reply(ctx, msg, sid, ToolsLine(c.cfg.Lang, n.Tools))
			}
		})
	}

	c.typing(ctx, msg, true)
	final, err := c.cfg.RunTurn(tctx, sid, msg.Text)
	c.typing(ctx, msg, false)
	if err != nil {
		if errors.Is(err, plugin.ErrSessionBusy) {
			c.send(ctx, msg, i18n.T(c.cfg.Lang, "imbot.busy",
				"当前会话正忙（可能有其他对话或后台任务在进行），请稍后再试。"))
			return
		}
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.err.turn", "处理失败：%s", err.Error()))
		return
	}
	if strings.TrimSpace(final) == "" {
		final = i18n.T(c.cfg.Lang, "imbot.no_reply", "（本轮没有文本回复）")
	}
	c.replyFinal(ctx, msg, sid, final)
}

// reply 把助手这一轮的产出投出去：默认原路回，装了路由且目标是别的通道时转投过去。
//
// 转投失败不回落到来源通道。分通道的全部意义就是让某一侧人格的话不出现在另一侧的
// 窗口里，回落等于把它直接摆过去，还是在人最不设防的时候。失败按丢弃处理，留一行
// 日志与一条会话注记——话本身仍在会话文件里，Web UI 翻得到。
//
// 只用于助手的产出。命令回执、错误提示、确认请求都归来源通道：那是对说话的人的
// 即时反馈，尤其确认请求的 pending 登记在来源通道的 broker 上，投到别处就没人能答。
//
// reply 用于过程通知（思考链、工具名），原样一条发出；replyFinal 用于本轮的最终
// 回复，按 HumanPace 分条带节奏——分条属于「角色在说话」，过程通知不是。
func (c *Core) reply(ctx context.Context, msg Message, sessionID, text string) {
	c.deliver(ctx, msg, sessionID, text, false)
}

func (c *Core) replyFinal(ctx context.Context, msg Message, sessionID, text string) {
	c.deliver(ctx, msg, sessionID, text, true)
}

func (c *Core) deliver(ctx context.Context, msg Message, sessionID, text string, paced bool) {
	target := Target(sessionID)
	if target == "" || target == c.cfg.PluginName {
		if paced {
			c.sendPaced(ctx, msg, text)
		} else {
			c.send(ctx, msg, text)
		}
		return
	}
	// 转投到别的通道时，分不分条由那条通道自己的开关决定：节奏是出口的属性
	if deliverTo(ctx, c.cfg.Lang, target, sessionID, text, paced) {
		return
	}
	i18n.Logf(c.cfg.Lang, "log.imbot.route_dropped", "%s: 本轮回复该发往 %s，那条通道投不出去，已丢弃", c.cfg.PluginName, target)
	c.notice(ctx, sessionID, i18n.Tf(c.cfg.Lang, "imbot.notice.undelivered",
		"有一段回复该发往「%s」，但那条通道当时投不出去，没能送达。", target))
}

// notice 往会话里留一行只给人看的说明；通道没提供 Notice 就只剩日志。
func (c *Core) notice(ctx context.Context, sessionID, text string) {
	if c.cfg.Notice == nil {
		return
	}
	if err := c.cfg.Notice(ctx, sessionID, text); err != nil {
		i18n.Logf(c.cfg.Lang, "log.imbot.notice_failed", "%s: 写会话注记失败: %v", c.cfg.PluginName, err)
	}
}

// typing 发「正在输入」状态，通道没实现就什么也不做。
func (c *Core) typing(ctx context.Context, msg Message, active bool) {
	if c.cfg.Typing != nil {
		c.cfg.Typing(ctx, msg, active)
	}
}

func (c *Core) cmdNew(ctx context.Context, msg Message) {
	sid, err := c.cfg.NewSession()
	if err != nil {
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.err.new_session", "新建会话失败：%s", err.Error()))
		return
	}
	if err := c.binding.Set(msg.UserID, sid); err != nil {
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.err.save_binding", "保存会话映射失败：%s", err.Error()))
		return
	}
	// 分通道时 /new 移动整组：其余通道在各自的下一条消息上加入这个新会话。
	// 只搬自己的话，另一条通道会留在旧会话里，两个人格当场分家。
	setGroupAnchor(sid)
	c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.new_session",
		"✨ 已新建会话（%s），接下来的消息将进入新会话。", sid))
}

// cmdStatus 的输出与 Web UI 的 /status 命令保持一致（同源数据，同样的措辞与格式）。
func (c *Core) cmdStatus(ctx context.Context, msg Message) {
	if c.cfg.Status == nil {
		c.send(ctx, msg, i18n.T(c.cfg.Lang, "imbot.no_status", "当前环境不支持状态查询。"))
		return
	}
	sid, err := c.SessionFor(msg.UserID)
	if err != nil {
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.err.session_init", "会话初始化失败：%s", err.Error()))
		return
	}
	info, err := c.cfg.Status(sid)
	if err != nil {
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.err.status", "获取状态失败：%s", err.Error()))
		return
	}
	// 措辞三处输出共用一份实现，见 internal/statustext
	c.send(ctx, msg, statustext.Render(c.cfg.Lang, info, sid))
}

func (c *Core) cmdCompact(ctx context.Context, msg Message) {
	if c.cfg.Compact == nil {
		c.send(ctx, msg, i18n.T(c.cfg.Lang, "imbot.no_compact", "当前环境不支持压缩。"))
		return
	}
	sid, err := c.SessionFor(msg.UserID)
	if err != nil {
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.err.session_init", "会话初始化失败：%s", err.Error()))
		return
	}
	c.send(ctx, msg, i18n.T(c.cfg.Lang, "imbot.compacting", "开始压缩会话历史，请稍候…"))
	tctx, cancel := context.WithTimeout(ctx, c.cfg.TurnTimeout)
	defer cancel()
	if err := c.cfg.Compact(tctx, sid); err != nil {
		c.send(ctx, msg, i18n.Tf(c.cfg.Lang, "imbot.err.compact", "压缩失败：%s", err.Error()))
		return
	}
	c.send(ctx, msg, i18n.T(c.cfg.Lang, "imbot.compacted", "📦 会话历史已压缩完成。"))
}

// ThinkingLine / ToolsLine 的措辞与 Web UI 的过程展示保持一致（🧠 思考过程 / 🔧 调用工具）。
// 工具名带行内代码标记：markdown 模式由客户端渲染成代码样式，纯文本模式转换为「工具名」。
func ThinkingLine(lang, text string) string {
	return i18n.Tf(lang, "imbot.thinking", "🧠 思考过程\n%s", strings.TrimSpace(text))
}

func ToolsLine(lang string, tools []string) string {
	quoted := make([]string, 0, len(tools))
	for _, t := range tools {
		quoted = append(quoted, "`"+t+"`")
	}
	return i18n.Tf(lang, "imbot.tools", "🔧 调用工具 %s",
		strings.Join(quoted, i18n.T(lang, "imbot.tools_sep", "、")))
}
