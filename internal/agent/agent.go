package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

type EventType string

const (
	EventDelta        EventType = "delta"         // 助手文本增量
	EventThinking     EventType = "thinking"      // 思考内容增量
	EventToolStart    EventType = "tool_start"    // 开始执行工具
	EventToolResult   EventType = "tool_result"   // 工具执行结果
	EventCompactStart EventType = "compact_start" // 自动压缩开始
	EventCompactDelta EventType = "compact_delta" // 自动压缩摘要增量
	EventCompactDone  EventType = "compact_done"  // 自动压缩结束（Error 非空表示失败）
	EventPrompt       EventType = "prompt"        // 本次 LLM 调用的完整请求体（仅在开启提示词追踪时发出）
	EventDone         EventType = "done"          // 本轮请求结束
	EventError        EventType = "error"
)

// autoCompactThreshold 会话上下文占用达到窗口的该比例时触发自动压缩。
const autoCompactThreshold = 0.90

// Event 是 Agent 运行过程中对外发布的事件（server 将其转为 SSE 帧）。
type Event struct {
	Type       EventType       `json:"type"`
	Content    string          `json:"content,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolArgs   json.RawMessage `json:"tool_args,omitempty"`
	ToolResult string          `json:"tool_result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Prompt     json.RawMessage `json:"prompt,omitempty"` // EventPrompt 专用：序列化后的请求体
}

type Options struct {
	Model        string
	Temperature  float64
	MaxTokens    int
	SystemPrompt string
	// Lang 是这个进程发给模型的文本用哪门语言（zh / en / …）。空串按中文处理。
	// 启动时定死、整轮之间不变，因此 system 前缀仍是逐字节稳定的缓存前缀。
	Lang          string
	MaxTurns      int
	Workdir       string // 工具的工作目录，写入环境块
	Thinking      string // 思考模式：off 或强度档位
	ContextLength int    // 模型上下文窗口（token），超出预算时裁剪最旧轮次
}

// ModelOptions 是可热切换的模型参数（与 SystemPrompt/MaxTurns/Workdir 等进程级配置分开）。
type ModelOptions struct {
	Model         string
	Temperature   float64
	MaxTokens     int
	Thinking      string
	ContextLength int
}

type Agent struct {
	mu       sync.RWMutex // 保护 provider 与 opts（模型可运行时切换）
	provider llm.Provider
	plugins  *plugin.Manager
	store    *session.Store
	opts     Options
	// noticeSink 是会话注记的实时出口（见 notice.go）。同受 mu 保护：
	// 它由 server 在启动时装一次，而注记从插件的后台 goroutine 上写出。
	noticeSink NoticeSink

	// 每会话一把轮次锁：同一会话的对话轮次与压缩互斥，防止交错写历史，
	// 以及压缩的 Replace 覆盖并发轮次刚 Append 的消息。
	lockMu    sync.Mutex
	turnLocks map[string]*sync.Mutex
}

func New(provider llm.Provider, plugins *plugin.Manager, store *session.Store, opts Options) *Agent {
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = 20
	}
	return &Agent{provider: provider, plugins: plugins, store: store, opts: opts, turnLocks: map[string]*sync.Mutex{}}
}

// turnLock 返回该会话的轮次锁（惰性创建）。
func (a *Agent) turnLock(sessionID string) *sync.Mutex {
	a.lockMu.Lock()
	defer a.lockMu.Unlock()
	l, ok := a.turnLocks[sessionID]
	if !ok {
		l = &sync.Mutex{}
		a.turnLocks[sessionID] = l
	}
	return l
}

// snapshot 取一份当前 provider 与配置。一轮请求全程使用同一份快照，
// 避免长流中途被切换导致模型与上下文（尤其思考块签名）错配。
func (a *Agent) snapshot() (llm.Provider, Options) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.provider, a.opts
}

// SetModel 热切换 provider 与模型参数，下一次请求生效；进行中的请求不受影响。
func (a *Agent) SetModel(p llm.Provider, m ModelOptions) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.provider = p
	a.opts.Model = m.Model
	a.opts.Temperature = m.Temperature
	a.opts.MaxTokens = m.MaxTokens
	a.opts.Thinking = m.Thinking
	a.opts.ContextLength = m.ContextLength
}

// Options 返回当前生效配置的副本。
func (a *Agent) Options() Options {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.opts
}

// Complete 用当前模型做一次一问一答的辅助调用：不带工具、不启用思考、不写入任何会话。
// 供插件完成提炼、归类这类内部工作，与用户对话相互独立。
func (a *Agent) Complete(ctx context.Context, prompt string) (string, error) {
	provider, opts := a.snapshot()
	events, err := provider.ChatStream(ctx, llm.ChatRequest{
		Model:       opts.Model,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Thinking:    "off",
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for ev := range events {
		switch ev.Type {
		case llm.EventContentDelta:
			out.WriteString(ev.Content)
		case llm.EventError:
			return "", ev.Err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return out.String(), nil
}

const titleMaxRunes = 30

// Run 处理一条用户消息：写入 session、驱动工具循环、通过 emit 实时发布事件。
// 返回前一定会 emit 一个 done 或 error 事件。
// 前台轮次排队等锁：用户在等着，报忙不如稍等（同会话并发本就是双开页面这类边缘情况）。
func (a *Agent) Run(ctx context.Context, sessionID, userInput string, emit func(Event)) {
	l := a.turnLock(sessionID)
	l.Lock()
	defer l.Unlock()
	if _, err := a.run(ctx, sessionID, userInput, emit); err != nil {
		emit(Event{Type: EventError, Error: err.Error()})
		return
	}
	emit(Event{Type: EventDone})
}

// RunTurn 供插件以编程方式发起一轮完整对话（经 InitContext.RunTurn 暴露），返回助手的最终文本。
// 后台轮次不排队：会话忙时立即返回 plugin.ErrSessionBusy，由发起方决定跳过或重试——
// 后台任务堆在锁上排队只会在解锁瞬间连环轰炸会话。
func (a *Agent) RunTurn(ctx context.Context, sessionID, input string) (string, error) {
	l := a.turnLock(sessionID)
	if !l.TryLock() {
		return "", plugin.ErrSessionBusy
	}
	defer l.Unlock()
	return a.run(ctx, sessionID, input, func(Event) {})
}

func (a *Agent) run(ctx context.Context, sessionID, userInput string, emit func(Event)) (string, error) {
	startedAt := time.Now()
	provider, opts := a.snapshot()

	meta, history, err := a.store.Get(sessionID)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	// 裁决本轮可见域。一轮只裁一次并全程从 ctx 取：若在工具执行阶段再裁一次，
	// 同一轮的 assistant 与 tool 结果可能落到不同标签，tool_use / tool_result 会被永久拆散。
	ev := plugin.TurnEvent{SessionID: sessionID, UserInput: userInput}
	scope := a.plugins.DecideScope(ctx, ev)
	ctx = plugin.WithScope(ctx, scope)
	ev.Scope = scope

	ephemeral := plugin.IsEphemeralInput(ctx)

	// 首条用户消息生成标题（连同它所属的可见域一起记下）。
	// 一次性输入不参与命名：心跳提示词不该成为会话标题。
	if meta.Title == "" && !ephemeral {
		title := []rune(userInput)
		if len(title) > titleMaxRunes {
			title = title[:titleMaxRunes]
		}
		_ = a.store.SetTitleTagged(sessionID, string(title), scope.Write)
	}

	origin := plugin.TurnOriginFrom(ctx)
	userMsg := llm.Message{Role: llm.RoleUser, Content: userInput}
	userKind := ""
	if ephemeral {
		userKind = session.KindEphemeral
	}
	if err := a.appendKind(sessionID, userMsg, scope.Write, origin, userKind); err != nil {
		return "", err
	}
	// 只有真人交互的轮次才算「活跃」：机器自发的轮次若也计入，
	// 以最近活跃会话为落点的后台功能会不断自我续命。
	if plugin.IsInteractive(ctx) {
		_ = a.store.SetLastActive(sessionID, startedAt)
	}

	// 组装上下文，分稳定段与易变段两层：
	//   system（环境块 + 工具约定 + 历史约定 + 启用插件的提示词片段 + 可选配置提示词）
	//   + 历史 + 本轮状态 + 本条
	// 易变的内容（当前时间、插件的每轮片段）**不进 system**，而是拼在本轮输入之前。
	// 一处改动解决两件事：模型判断「现在」时不必跨几千 token 主动比对，
	// 且 system 与历史成为整轮之间字节一致的前缀，提示词缓存才可能命中。
	msgs := make([]llm.Message, 0, len(history)+2)
	parts := []string{envContext(opts.Lang, opts.Workdir), toolRulesFor(opts.Lang), historyRulesFor(opts.Lang)}
	parts = append(parts, a.plugins.SystemPrompts()...)
	if opts.SystemPrompt != "" {
		parts = append(parts, opts.SystemPrompt)
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: strings.Join(parts, "\n\n")})
	// pinned 与 msgs 一一对应，标记预算裁剪时不能丢的消息（system 与压缩摘要）
	pinned := []bool{true}
	// 历史里补上时间的信号，两种：日期翻篇处标一行绝对日期（dayMark），隔得久的地方
	// 标一行间隔（gapNote）。原文不变，只改发出去的副本。
	//
	// 落点分两种。用户消息直接前置，这是既有做法。角色自己开口的那一轮没有用户消息可
	// 依附——心跳注入的提示是一次性输入，后续上下文里已被剔掉——于是单独插一条程序
	// 标注。那类主动开口的话此前一点时间信号都没有，而「昨天说的明天」多半正是那么说
	// 出口的：第二天读到它时，没有任何东西表明那句话不是刚说的。
	var lastTS time.Time
	var markedDay string
	// pending 是还没找到落点的标注。工具结果那几条中间插不得：Anthropic 侧连续的
	// user 消息会合并成一条，tool_result block 必须排在最前，中间夹一段文字就把它们
	// 拆散了。跨零点的那种轮次因此把标注顺延到下一个能插的位置——历史正好以工具结果
	// 结尾时（轮次被中断）就丢掉，那种残缺的尾巴不值得为它多开一条路径。
	var pending string
	breakable := true // 上一条消息有没有还挂着的工具调用
	for _, h := range visibleHistory(history, scope) {
		m := h.Message
		note := pending
		pending = ""
		if !h.TS.IsZero() {
			if d := h.TS.Format("2006-01-02"); d != markedDay {
				markedDay = d
				note += dayMark(opts.Lang, h.TS) + "\n"
			}
		}
		note += gapNote(opts.Lang, lastTS, h.TS)
		if !h.TS.IsZero() {
			lastTS = h.TS
		}
		switch {
		case note == "":
		case m.Role == llm.RoleUser:
			m.Content = note + m.Content
		case m.Role == llm.RoleTool || !breakable:
			pending = note
		default:
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: strings.TrimRight(note, "\n")})
			pinned = append(pinned, false)
		}
		breakable = len(m.ToolCalls) == 0
		msgs = append(msgs, m)
		pinned = append(pinned, h.Kind == session.KindSummary)
	}
	// 本轮状态块拼在用户输入前面，落盘的那条（userMsg）保持原样不受影响
	wireUser := userMsg
	var sinceLast time.Duration
	if !lastTS.IsZero() {
		sinceLast = startedAt.Sub(lastTS)
	}
	wireUser.Content = turnStateBlock(opts.Lang, startedAt, sinceLast, a.plugins.TurnPrompts(ctx, ev)) +
		"\n\n" + userMsg.Content
	msgs = append(msgs, wireUser)
	pinned = append(pinned, false)

	var guard toolGuard
	for turn := 0; turn < opts.MaxTurns; turn++ {
		r, err := a.stream(ctx, provider, opts, msgs, pinned, emit)
		if err != nil {
			// 给插件一个把失败转成正常回复的机会（如角色扮演场景把内容拦截
			// 演成一句走神）。转译成功即按正常轮次收尾，原始错误进会话注记。
			text, ok := a.translateFailure(ctx, sessionID, scope.Write, origin, r.content, err, emit)
			if !ok {
				return "", err
			}
			a.plugins.NotifyTurnEnd(ctx, plugin.TurnEndEvent{
				SessionID:   sessionID,
				Origin:      origin,
				Interactive: true, // 转译只发生在真人在场的轮次
				UserInput:   userInput,
				FinalText:   text,
				StartedAt:   startedAt,
				EndedAt:     time.Now(),
			})
			return text, nil
		}
		if r.usage != nil {
			// 记录实测用量（供 /status 展示与自动压缩判断）
			_ = a.store.SetUsage(sessionID, &session.Usage{
				PromptTokens:     r.usage.PromptTokens,
				CompletionTokens: r.usage.CompletionTokens,
				CachedTokens:     r.usage.CachedTokens,
				CacheWriteTokens: r.usage.CacheWriteTokens,
			})
		}

		// reasoning 一并持久化并留在上下文里：带工具调用时 DeepSeek 要求回传，
		// Anthropic 则要求原样回传带签名的思考块（reasoningBlocks）
		assistant := llm.Message{
			Role:            llm.RoleAssistant,
			Content:         r.content,
			Reasoning:       r.reasoning,
			ReasoningBlocks: r.blocks,
			ToolCalls:       r.toolCalls,
		}
		if err := a.append(sessionID, assistant, scope.Write, origin); err != nil {
			return "", err
		}
		msgs = append(msgs, assistant)
		pinned = append(pinned, false)

		// 过程通知：每轮模型响应的完整思考链与工具调用批次（发起方装了回调才有）。
		// 工具通知只带名字——参数与结果可能载有隐私，转发与否不该由核心替接收方决定。
		if noteFn := plugin.TurnNotesFrom(ctx); noteFn != nil {
			if strings.TrimSpace(r.reasoning) != "" {
				noteFn(plugin.TurnNote{Kind: plugin.NoteThinking, Text: r.reasoning})
			}
			if len(r.toolCalls) > 0 {
				names := make([]string, 0, len(r.toolCalls))
				for _, call := range r.toolCalls {
					names = append(names, call.Name)
				}
				noteFn(plugin.TurnNote{Kind: plugin.NoteToolCalls, Tools: names})
			}
		}

		if len(r.toolCalls) == 0 {
			// 无工具调用，本轮结束
			a.maybeAutoCompact(ctx, provider, opts, sessionID, r.usage, emit)
			a.plugins.NotifyTurnEnd(ctx, plugin.TurnEndEvent{
				SessionID:   sessionID,
				Origin:      origin,
				Interactive: plugin.IsInteractive(ctx),
				UserInput:   userInput,
				FinalText:   r.content,
				StartedAt:   startedAt,
				EndedAt:     time.Now(),
			})
			return r.content, nil
		}

		for _, call := range r.toolCalls {
			emit(Event{Type: EventToolStart, ToolCallID: call.ID, ToolName: call.Name, ToolArgs: call.Arguments})
			// 护栏先看一眼：重复的调用不执行，把「已经做完了」当场告诉模型
			result := guard.check(a.opts.Lang, call)
			if result == "" {
				result = a.execute(ctx, call)
			} else {
				log.Printf("agent: 拦下重复的工具调用 %s（本轮第 %d 次拦截）", call.Name, guard.hits)
			}
			emit(Event{Type: EventToolResult, ToolCallID: call.ID, ToolName: call.Name, ToolResult: result})

			toolMsg := llm.Message{Role: llm.RoleTool, Content: result, ToolCallID: call.ID}
			if err := a.append(sessionID, toolMsg, scope.Write, origin); err != nil {
				return "", err
			}
			msgs = append(msgs, toolMsg)
			pinned = append(pinned, false)
		}

		// 护栏拦了几次仍不收尾：这一轮不会再有进展，结束它。走的是失败路径而不是
		// 「就地收尾」——前台由 FailureTranslator 演成一句走神，后台的发起方（心跳、
		// 日程）据此记一次失败并按自己的节奏重来，两边都比返回一句空话强。
		if guard.exhausted() {
			log.Printf("agent: 会话 %s 的轮次因工具反复调用被中止", sessionID)
			return "", toolLoopError(a.opts.Lang)
		}
	}
	return "", fmt.Errorf("reached max_turns (%d) without a final answer", opts.MaxTurns)
}

// turnResult 是一次 LLM 调用的完整产出。
type turnResult struct {
	content   string
	reasoning string
	blocks    []llm.ReasoningBlock
	toolCalls []llm.ToolCall
	usage     *llm.Usage
}

// stream 发起一次 LLM 调用，转发文本与思考增量，返回本轮完整产出。
func (a *Agent) stream(ctx context.Context, provider llm.Provider, opts Options, msgs []llm.Message, pinned []bool, emit func(Event)) (turnResult, error) {
	var r turnResult
	req := llm.ChatRequest{
		Model:       opts.Model,
		Messages:    trimToBudget(opts, msgs, pinned),
		Tools:       a.toolSpecs(),
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Thinking:    opts.Thinking,
	}
	// 调试用：发出本次调用的完整请求体。序列化失败不影响对话，静默跳过即可
	if promptTraceEnabled(ctx) {
		if raw, err := marshalPromptTrace(req); err == nil {
			emit(Event{Type: EventPrompt, Prompt: raw})
		}
	}
	events, err := provider.ChatStream(ctx, req)
	if err != nil {
		return turnResult{}, err
	}

	for ev := range events {
		switch ev.Type {
		case llm.EventContentDelta:
			r.content += ev.Content
			emit(Event{Type: EventDelta, Content: ev.Content})
		case llm.EventReasoningDelta:
			r.reasoning += ev.Content
			emit(Event{Type: EventThinking, Content: ev.Content})
		case llm.EventToolCalls:
			r.toolCalls = ev.ToolCalls
		case llm.EventReasoning:
			r.blocks = ev.Reasoning
		case llm.EventUsage:
			r.usage = ev.Usage
		case llm.EventError:
			// 半截产出随错误一起交回：内容不作数（调用方不落盘），
			// 但界面上增量已经流出去了，失败善后需要知道这一点。
			return r, ev.Err
		}
	}
	if err := ctx.Err(); err != nil {
		return turnResult{}, err
	}
	return r, nil
}

// translateFailure 在模型调用失败后给插件一个把失败转成一句正常回复的机会
// （经 Manager.TranslateFailure，单所有者）。成功时：文本以增量事件送出、以正常
// 助手消息落盘（带本轮可见域标签），原始错误转入会话注记——注记只给人看、
// 永不进模型上下文，真相因此不丢。
//
// 只在真人在场的轮次尝试：后台轮次没人看，转译白费一次模型调用，还会把失败
// 伪装成成功、干扰发起方（如心跳按报错跳过本拍）的判断。用户主动取消（ctx
// 已作废）也不转译——那不是失败，而且转译自己也需要这个 ctx。
//
// partial 是失败前已经流出去的半截文本：它不落盘（与失败路径一致），但界面上
// 已经显示了，转译文本得另起一段接在后面，避免两段话粘成一句。
func (a *Agent) translateFailure(ctx context.Context, sessionID, tag, origin, partial string, cause error, emit func(Event)) (string, bool) {
	if !plugin.IsInteractive(ctx) || ctx.Err() != nil {
		return "", false
	}
	text, ok := a.plugins.TranslateFailure(ctx, plugin.TurnFailure{SessionID: sessionID, Err: cause})
	if !ok || text == "" {
		return "", false
	}
	out := text
	if strings.TrimSpace(partial) != "" {
		out = "\n\n" + text
	}
	emit(Event{Type: EventDelta, Content: out})
	if err := a.append(sessionID, llm.Message{Role: llm.RoleAssistant, Content: text}, tag, origin); err != nil {
		// 落不了盘就退回原始错误：界面已经流出的台词与历史会不一致，
		// 但那是磁盘故障级别的场景，一致性让位于把问题报出来。
		return "", false
	}
	note := i18n.Tf(a.opts.Lang, "agent.notice.failure_translated",
		"本轮模型调用失败，回复由插件转译生成。原始错误：%s", cause.Error())
	if err := a.AppendNotice(ctx, sessionID, note); err != nil {
		log.Printf("写失败转译注记失败: %v", err)
	}
	return text, true
}

// maybeAutoCompact 在上下文占用达到窗口阈值时自动压缩会话。
//
// 判据取「实测用量」与「全量历史估算」的较大者。实测用量只反映本轮实际发出的、
// 已按可见域过滤过的上下文；只看它的话，一个上下文很小的可见域会让整个会话永不触发
// 压缩，而其它可见域的历史仍在无限增长、记忆提炼也永不发生。偏早压缩无害——压缩本来
// 就是上下文溢出时的保底手段。
func (a *Agent) maybeAutoCompact(ctx context.Context, provider llm.Provider, opts Options, sessionID string, usage *llm.Usage, emit func(Event)) {
	if opts.ContextLength <= 0 {
		return
	}
	used := 0
	if usage != nil {
		used = usage.PromptTokens + usage.CompletionTokens
	}
	if _, history, err := a.store.Get(sessionID); err == nil {
		if est := EstimateHistoryTokens(messagesOf(history)); est > used {
			used = est
		}
	}
	if float64(used) < float64(opts.ContextLength)*autoCompactThreshold {
		return
	}
	emit(Event{Type: EventCompactStart})
	wrapped := func(ev Event) {
		if ev.Type == EventDelta {
			emit(Event{Type: EventCompactDelta, Content: ev.Content})
		}
	}
	if err := a.compact(ctx, provider, opts, sessionID, wrapped); err != nil {
		emit(Event{Type: EventCompactDone, Error: err.Error()})
		return
	}
	emit(Event{Type: EventCompactDone})
}

// estimateTokens 粗略估算消息 token 数：UTF-8 字节数 / 3
// （中文约 1 字 1 token，对英文略偏高，作为安全裕量）。
func estimateTokens(m llm.Message) int {
	n := len(m.Content) + len(m.Reasoning)
	for _, tc := range m.ToolCalls {
		n += len(tc.Name) + len(tc.Arguments)
	}
	return n/3 + 8
}

// trimToBudget 在超出上下文预算时，从最旧的对话轮次（以 user 消息开头）开始整轮丢弃，
// 始终保留 system 消息与最近的轮次，保证 tool_calls 与 tool 结果不会被拆散。
//
// pinned 与 msgs 一一对应，标记不可丢弃的消息；含被钉住消息的轮次会被跳过，继续找
// 更近的轮次来丢。压缩摘要要钉住：它代表的是一大段已被物理删除的历史，丢掉它等于
// 永久失忆，代价远高于丢一轮普通对话。pinned 可以为 nil（等同全部不钉）。
func trimToBudget(opts Options, msgs []llm.Message, pinned []bool) []llm.Message {
	budget := opts.ContextLength - opts.MaxTokens - 2048 // 预留输出与协议开销
	if budget <= 0 {
		return msgs
	}
	total := 0
	for _, m := range msgs {
		total += estimateTokens(m)
	}
	if total <= budget || len(msgs) < 2 {
		return msgs
	}

	isPinned := func(i int) bool { return i < len(pinned) && pinned[i] }
	// msgs[0] 为 system，永不参与裁剪；其后按 user 消息切分轮次
	turns := turnStarts(msgs)

	drop := make([]bool, len(msgs))
	// 最后一轮（当前轮）不能丢
	for ti := 0; ti+1 < len(turns) && total > budget; ti++ {
		start, end := turns[ti], turns[ti+1]
		skip := false
		for i := start; i < end; i++ {
			if isPinned(i) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		for i := start; i < end; i++ {
			drop[i] = true
			total -= estimateTokens(msgs[i])
		}
	}

	out := make([]llm.Message, 0, len(msgs))
	for i, m := range msgs {
		if !drop[i] {
			out = append(out, m)
		}
	}
	return out
}

// turnStarts 返回各轮次在 msgs 中的起始下标，末尾附上 len(msgs) 作为哨兵。
// 轮次以 user 消息为界；msgs[0] 是 system，不算在内。
func turnStarts(msgs []llm.Message) []int {
	var starts []int
	for i := 1; i < len(msgs); i++ {
		// 第一条无论角色都作为首轮起点：历史可能以非 user 消息开头
		if len(starts) == 0 || msgs[i].Role == llm.RoleUser {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return nil
	}
	return append(starts, len(msgs))
}

// toolSpecs 每次请求时从启用插件动态生成工具声明（运行时开关天然生效）。
func (a *Agent) toolSpecs() []llm.ToolSpec {
	enabled := a.plugins.EnabledTools()
	specs := make([]llm.ToolSpec, 0, len(enabled))
	for _, t := range enabled {
		specs = append(specs, llm.ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		})
	}
	return specs
}

func (a *Agent) execute(ctx context.Context, call llm.ToolCall) string {
	tool, ok := a.plugins.FindTool(call.Name)
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", call.Name)
	}
	result, err := tool.Execute(ctx, call.Arguments)
	if err != nil {
		// 错误也作为结果反馈给模型，让其决定下一步
		return fmt.Sprintf("error: %v", err)
	}
	if result == "" {
		result = "(empty output)"
	}
	return result
}

// append 落盘一条消息。tag 是本轮的可见域标签，一轮内的 user / assistant / tool
// 消息必须使用同一个值，否则 tool_use 与 tool_result 会被过滤拆散。
// origin 是本轮的发起方（同样整轮一致），空串 = 前台界面。
func (a *Agent) append(sessionID string, msg llm.Message, tag, origin string) error {
	return a.appendKind(sessionID, msg, tag, origin, "")
}

func (a *Agent) appendKind(sessionID string, msg llm.Message, tag, origin, kind string) error {
	return a.store.Append(sessionID, session.StoredMessage{Message: msg, Kind: kind, Tag: tag, Origin: origin, TS: time.Now()})
}
