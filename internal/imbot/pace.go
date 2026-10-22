package imbot

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 出站的「像人一样发消息」：回复拆成几条、带打字间隔陆续发出，并在本通道发起的
// 轮次里提醒模型用聊天的口吻说话。三件事归一个开关（Config.HumanPace），因为
// 它们是一体的——光拆不改口吻，拆出来的是六条论文段落；光改口吻不拆，一屏短句
// 堆在一条消息里也不像人。
//
// 只作用于助手的产出（本轮最终回复、后台轮次的推送）。命令回执、错误提示、确认
// 请求与过程通知都是对操作者的即时反馈，原样一条发出，不分条也不延迟。

// 分条与节奏的参数。做成常量不暴露：可调的东西越多，用户越要理解机制才敢开。
const (
	segmentRunes = 120 // 每条的字数上限（按 rune 计）
	maxSegments  = 6   // 一轮最多拆几条，超出的并进最后一条

	typingPerRune  = 60 * time.Millisecond // 模拟打字：每个字的耗时
	minTypingDelay = 400 * time.Millisecond
	maxTypingDelay = 2500 * time.Millisecond
)

// ChatStylePrompt 是开着 HumanPace 时注入本通道轮次的说话引导，五条通道共用一份。
// 随 <本轮状态> 走、不进 system，且只在本通道发起的轮次里出现：Web UI 与 CLI 的
// 轮次不受影响。
const chatStylePromptZH = "[对话渠道]\n这是在即时通讯软件里的聊天：像发消息那样说话——短句、口语，一次一两句就好，不写长篇；不用标题、列表与加粗。"

// ChatStylePrompt 按语言取出这段引导。
func ChatStylePrompt(lang string) string {
	return prompt.T(lang, "prompt.imbot.chat_style", chatStylePromptZH)
}

// TurnPrompt 返回本轮该注入的片段，只在本通道发起的轮次里出现：开着「像人一样发消息」
// 时的说话引导，以及被扣过的消息放行那一轮的「回晚了」（见 hold.go）。都没有时为空串。
// 通道插件的 TurnPrompt 直接转交给它即可。
func (c *Core) TurnPrompt(ctx context.Context) string {
	if plugin.TurnOriginFrom(ctx) != c.cfg.PluginName {
		return ""
	}
	var parts []string
	if c.cfg.HumanPace {
		parts = append(parts, ChatStylePrompt(c.cfg.Lang))
	}
	if note := c.heldPrompt(ctx); note != "" {
		parts = append(parts, note)
	}
	return strings.Join(parts, "\n\n")
}

// PushReply 把一段「角色在说话」的文本主动推给某个用户（后台轮次的结果落在绑定
// 会话上时用），按 HumanPace 决定是否分条带节奏。返回是否真的交给了平台。
func (c *Core) PushReply(ctx context.Context, userID, text string) bool {
	if c.cfg.Push == nil {
		return false
	}
	return c.pushPaced(ctx, userID, text)
}

// sendPaced 把助手的回复原路发给说话的人，按开关分条。
func (c *Core) sendPaced(ctx context.Context, msg Message, text string) {
	c.paced(ctx, text,
		func(s string) bool { c.send(ctx, msg, s); return true },
		func(on bool) { c.typing(ctx, msg, on) })
}

// pushPaced 经主动推送通道把助手的回复发给某个用户，按开关分条。
// 主动推送没有入站消息可回带，Typing 钩子只拿得到用户标识。
func (c *Core) pushPaced(ctx context.Context, userID, text string) bool {
	var typing func(bool)
	if c.cfg.Typing != nil {
		typing = func(on bool) { c.cfg.Typing(ctx, Message{UserID: userID}, on) }
	}
	return c.paced(ctx, text,
		func(s string) bool { return c.cfg.Push(ctx, userID, s) },
		typing)
}

// paced 是分条发送的公共部分：开关关着就原样一条；开着就切成几条，每条之前按
// 上一条的字数等一会儿（期间亮着「正在输入」），再发下一条。ctx 取消时停发剩余的。
func (c *Core) paced(ctx context.Context, text string, send func(string) bool, typing func(bool)) bool {
	if !c.cfg.HumanPace {
		return send(text)
	}
	segs := Segment(text)
	ok := false
	for i, s := range segs {
		if i > 0 {
			if typing != nil {
				typing(true)
			}
			c.sleep(ctx, paceDelay(utf8.RuneCountInString(segs[i-1])))
			if typing != nil {
				typing(false)
			}
			if ctx.Err() != nil {
				return ok
			}
		}
		if send(s) {
			ok = true
		}
	}
	return ok
}

// sleep 等一段时间，ctx 取消时提前返回；测试可经 Config.sleep 注入。
func (c *Core) sleep(ctx context.Context, d time.Duration) {
	if c.cfg.sleep != nil {
		c.cfg.sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// paceDelay 按字数算打字间隔：每字 60ms，最短 400ms（再短就看不出是两条），
// 最长 2.5s（再长就像掉线了）。
func paceDelay(runes int) time.Duration {
	d := time.Duration(runes) * typingPerRune
	if d < minTypingDelay {
		return minTypingDelay
	}
	if d > maxTypingDelay {
		return maxTypingDelay
	}
	return d
}

// Segment 把一段回复切成适合逐条发送的几段：先按空行分段落（代码块与表格整段
// 保留，不从中间切开），段落内再按句末标点切成句子、合并到每条不超过
// segmentRunes 个字；最多 maxSegments 条，超出的并进最后一条。
//
// 段落是硬边界：两段话在聊天软件里就是两条消息，哪怕都很短。句子是软边界：
// 同一段里的几句能装下就装在一条里。单句超长也不硬切——平台长度上限由通道的
// 分段兜底，这里只管读起来像不像人。
func Segment(text string) []string {
	return segment(text, segmentRunes, maxSegments)
}

// segText 是一条待发的分段，newPara 记录它与前一条之间原本是不是段落边界，
// 触顶合并时据此决定用空行还是直接相连。
type segText struct {
	text    string
	newPara bool
}

func segment(text string, maxRunes, maxSegs int) []string {
	var segs []segText
	cur := segText{newPara: true}
	flush := func() {
		if t := strings.TrimSpace(cur.text); t != "" {
			segs = append(segs, segText{text: t, newPara: cur.newPara})
		}
		cur = segText{}
	}
	for _, b := range blocks(text) {
		if b.atomic {
			flush()
			cur = segText{text: b.text, newPara: true}
			flush()
			continue
		}
		for i, s := range splitSentences(b.text) {
			if i == 0 {
				flush()
				cur = segText{text: s, newPara: true}
				continue
			}
			if cur.text != "" && utf8.RuneCountInString(cur.text)+utf8.RuneCountInString(s) > maxRunes {
				flush()
				cur = segText{text: s, newPara: false}
				continue
			}
			cur.text += s
		}
	}
	flush()

	if len(segs) > maxSegs {
		var b strings.Builder
		for i, s := range segs[maxSegs-1:] {
			if i > 0 {
				if s.newPara {
					b.WriteString("\n\n")
				}
			}
			b.WriteString(s.text)
		}
		segs = append(segs[:maxSegs-1], segText{text: b.String(), newPara: segs[maxSegs-1].newPara})
	}

	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, s.text)
	}
	if len(out) == 0 {
		out = []string{strings.TrimSpace(text)}
	}
	return out
}

// block 是按空行切出的一个段落；atomic 表示它是代码块或表格，整段不可再切。
type block struct {
	text   string
	atomic bool
}

// blocks 按空行切段落，围栏代码块与连续的表格行各自成为一个不可切的整块。
func blocks(text string) []block {
	var out []block
	var cur []string
	kind := "" // "" / "para" / "table" / "code"
	flush := func() {
		if len(cur) > 0 {
			out = append(out, block{text: strings.Join(cur, "\n"), atomic: kind == "code" || kind == "table"})
		}
		cur, kind = nil, ""
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if kind == "code" {
			cur = append(cur, line)
			if strings.HasPrefix(trimmed, "```") {
				flush()
			}
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "```"):
			flush()
			kind = "code"
			cur = append(cur, line)
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, "|"):
			if kind != "table" {
				flush()
				kind = "table"
			}
			cur = append(cur, line)
		default:
			if kind == "table" {
				flush()
			}
			kind = "para"
			cur = append(cur, line)
		}
	}
	flush()
	return out
}

// splitSentences 在句末标点之后切开一段文字，标点（连同紧随的引号、括号）留在
// 前一句末尾。认中文的 。！？…～ 与西文的 !?；西文句点只在后面跟着空白或到
// 了结尾、且前面不是数字时才算句末——「3.5」「1. 第一项」不是句子的结束。
func splitSentences(s string) []string {
	var out []string
	runes := []rune(s)
	start := 0
	for i := 0; i < len(runes); i++ {
		if !isSentenceEnd(runes, i) {
			continue
		}
		// 吞掉连着的标点与收尾的引号、括号：「？！」「……」「。」」都是一句的结尾
		j := i + 1
		for j < len(runes) && (isTerminal(runes[j]) || isCloser(runes[j])) {
			j++
		}
		out = append(out, string(runes[start:j]))
		start = j
		i = j - 1
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	if len(out) == 0 {
		out = []string{s}
	}
	return out
}

func isTerminal(r rune) bool {
	switch r {
	case '。', '！', '？', '…', '～', '!', '?':
		return true
	}
	return false
}

func isCloser(r rune) bool {
	switch r {
	case '」', '』', '”', '’', '）', ')', '"', '\'', '】', '〕', '》':
		return true
	}
	return false
}

func isSentenceEnd(runes []rune, i int) bool {
	r := runes[i]
	if isTerminal(r) {
		return true
	}
	if r != '.' {
		return false
	}
	if i > 0 && unicode.IsDigit(runes[i-1]) {
		return false
	}
	return i+1 == len(runes) || unicode.IsSpace(runes[i+1])
}

// 五条通道共用的两个配置项：键、文案与默认值只写一份，通道把它们拼进自己的
// ConfigFields，再用 PaceConfig 读回来交给 Config。
const (
	// DefaultMergeWindow 是合并窗口的默认值。3 秒是「敲完一句、正在敲下一句」的
	// 典型间隔：再短并不住连发，再长对方会觉得没人理。
	DefaultMergeWindow = 3 * time.Second

	keyMergeWindow = "merge_window_sec"
	keyHumanPace   = "human_pace"
	keyBusyHold    = "busy_hold"
	keyWakeAfter   = "wake_after_minutes"
)

// PaceFields 返回收发节奏的四个配置项声明：合并连发消息、像人一样发消息、
// 忙时晚点回、睡着时多久被吵醒。
func PaceFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key: keyMergeWindow, Label: "合并连发消息（秒）", Type: plugin.FieldInt,
			Default: int(DefaultMergeWindow / time.Second), Min: plugin.IntPtr(0), Max: plugin.IntPtr(30),
			Description: "同一个人在这段时间内连发的几条消息合成一轮再回复，像真人等对方说完；每来一条重新计时，总等待不超过三倍。0 关闭，逐条回复。命令不受影响",
		},
		{
			Key: keyHumanPace, Label: "像人一样发消息", Type: plugin.FieldBool, Default: false,
			Description: "回复按句子分成几条、带打字间隔陆续发出，并提示模型用聊天的口吻说话（短句、口语、不排版）。命令回执、确认请求与过程通知仍原样一条",
		},
		{
			Key: keyBusyHold, Label: "忙时晚点回", Type: plugin.FieldBool, Default: false,
			Description: "角色按日程正忙或睡着时，消息先收着、过一阵再回：重忙时最多等十分钟（抽空看一眼），" +
				"完全顾不上时等到忙完，睡着时按下面的时长算被吵醒。回的那一轮会告诉角色消息是多久前发来的。命令不受影响",
		},
		{
			Key: keyWakeAfter, Label: "睡着时多久被吵醒（分钟）", Type: plugin.FieldInt,
			Default: int(DefaultWakeAfter / time.Minute), Min: plugin.IntPtr(0), Max: plugin.IntPtr(720),
			Description: "开着「忙时晚点回」时，角色睡着后收到消息，攒这么久才算把人吵醒；到点自然醒得更早就随之回。0 表示一来消息就醒，调得很长就近似「等到睡醒再回」",
		},
	}
}

// HoldConfig 从插件配置里读出「忙时晚点回」开关与「多久被吵醒」。
func HoldConfig(cfg map[string]any) (holdWhenBusy bool, wakeAfter time.Duration) {
	return plugin.CfgBool(cfg, keyBusyHold, false),
		time.Duration(plugin.CfgInt(cfg, keyWakeAfter, int(DefaultWakeAfter/time.Minute))) * time.Minute
}

// PaceConfig 从插件配置里读出合并窗口与分条开关。
func PaceConfig(cfg map[string]any) (mergeWindow time.Duration, humanPace bool) {
	return time.Duration(plugin.CfgInt(cfg, keyMergeWindow, int(DefaultMergeWindow/time.Second))) * time.Second,
		plugin.CfgBool(cfg, keyHumanPace, false)
}
