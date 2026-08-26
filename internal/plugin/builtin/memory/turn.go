package memory

import (
	"context"
	"log"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 本文件是「对话进行中定期提炼」。压缩那条路径信息量足但触发得太少——大部分对话
// 走不到压缩就结束了，那些结论只能留在原始会话里靠检索兜底。这里每隔若干轮真人
// 对话提炼一次，代价是每个窗口多一次模型调用。
//
// 攒的是「用户输入 + 助手最终文本」，不是去读会话文件：压缩会用 Replace 物理重写
// 历史，任何基于消息序号的水位在那之后都失效，还要再写一套检测与复位。代价是丢掉
// 工具调用的细节，这一样由压缩那条路径兜住。
//
// 缓冲本身落盘（见 window_state.go）。它原本只在内存里，代价记作「进程重启丢缓冲」
// 并同样指望压缩兜住，但那兜不住：上下文窗口是百万级，压缩要到九成才触发，大多数
// 会话一辈子走不到那里。于是重启比提炼间隔更频繁的人，定期提炼一次都不会发生。

// windowKey 把提炼窗口按会话与可见域分开。同一个会话在两个可见域下的对话各攒各的，
// 提炼时也各写各的库。
type windowKey struct {
	session string
	tag     string // 可见域的写入标签，空串 = 共享
}

// windowTurn 是窗口里的一轮对话。
type windowTurn struct {
	user  string
	reply string
}

// window 是一段尚未提炼的对话。
type window struct {
	scope   plugin.Scope // 提炼时据此决定读哪些库、写哪个库
	turns   []windowTurn
	bytes   int
	lastEnd time.Time // 最后一轮的结束时刻，用来算「隔了多久」
}

func (w *window) add(ev plugin.TurnEndEvent, maxTurns int) {
	t := windowTurn{user: ev.UserInput, reply: ev.FinalText}
	w.turns = append(w.turns, t)
	w.bytes += len(t.user) + len(t.reply)
	w.lastEnd = ev.EndedAt
	// 提炼卡住或一直不够格时窗口会一直长，给它一个上界；丢最旧的那几轮，
	// 因为提炼的价值集中在离现在最近的对话上。
	for len(w.turns) > maxTurns {
		w.bytes -= len(w.turns[0].user) + len(w.turns[0].reply)
		w.turns = w.turns[1:]
	}
}

// text 把窗口渲染成送去提炼的对话文本。
func (w *window) text(lang string, maxBytes int) string {
	var b strings.Builder
	for _, t := range w.turns {
		if s := strings.TrimSpace(t.user); s != "" {
			b.WriteString(prompt.Tf(lang, "prompt.memory.dialogue.user", "用户: %s\n", s))
		}
		if s := strings.TrimSpace(t.reply); s != "" {
			b.WriteString(prompt.Tf(lang, "prompt.memory.dialogue.assistant", "助手: %s\n", s))
		}
	}
	return clampDialogue(lang, b.String(), maxBytes)
}

// OnTurnEnd 累计真人对话并在到点时触发提炼，顺带按天触发一次淡忘清扫。
// 本方法在轮次收尾的同步路径上被调用，必须快速返回。
func (p *Plugin) OnTurnEnd(ctx context.Context, ev plugin.TurnEndEvent) {
	// 只记真人在对面的轮次。这里刻意不按 Origin 过滤：远程 IM 的轮次 Origin 非空，
	// 但那头坐着的是真人，恰恰最该记；心跳与定时任务 Interactive 为假，自动被挡在
	// 外面——否则一个挂着心跳的会话会把自己的独白提炼成记忆。
	if !ev.Interactive {
		return
	}
	s := p.snapshot()
	if s.store == nil || s.ctx == nil {
		return
	}
	// 可见域必须当场从 ctx 取：广播用的是本轮的 ctx，它在轮次返回后立即被取消，
	// 之后再问就什么也拿不到了。
	scope := plugin.ScopeFrom(ctx)
	key := windowKey{session: ev.SessionID, tag: scope.Write}

	p.turnMu.Lock()
	if p.stopped {
		p.turnMu.Unlock()
		return
	}
	var ripe *window
	if s.turnExtract {
		ripe = p.accumulateLocked(key, scope, ev, s)
		if ripe != nil {
			if p.extracting == nil {
				p.extracting = map[windowKey]bool{}
			}
			p.extracting[key] = true
		}
	}
	// 遗忘是以天计的过程，一天扫一次足够，不值得为它起定时器；时间线的日切同理
	sweep := s.decay && !p.sweeping && !sameDay(p.lastSweep, ev.EndedAt)
	if sweep {
		p.sweeping, p.lastSweep = true, ev.EndedAt
	}
	dayFlush := s.timeline && !p.timelining && !sameDay(p.lastTimeline, ev.EndedAt)
	if dayFlush {
		p.timelining, p.lastTimeline = true, ev.EndedAt
	}
	if ripe == nil && !sweep && !s.turnExtract && !s.timeline {
		p.turnMu.Unlock()
		return
	}
	// 缓冲进展要落盘，否则重启就归零，「每 N 轮提炼一次」在重启比 N 轮更频繁时
	// 永远走不完。快照在锁内取，写盘在锁外做。
	p.windowSeq++
	dir, snapshot, seq := p.windowDir, p.snapshotWindowsLocked(), p.windowSeq
	marks := dayMarks{lastSweep: p.lastSweep, lastTimeline: p.lastTimeline}
	// 登记在锁内完成：Stop 会先在同一把锁下置 stopped 再 Wait，
	// 这样不会出现「刚 Add 完就被略过」的 goroutine
	p.wg.Add(1)
	p.turnMu.Unlock()

	go func() {
		defer p.wg.Done()
		// 先收束昨天，再把本轮追加进今天的缓冲——反过来会把新一天的第一轮
		// 也算进「昨天」。多天缓冲对并发追加是安全的：收束只取早于今天的日子。
		if dayFlush {
			p.runTimeline(s)
		}
		if s.timeline {
			p.appendDayBuf(s, scope.Write, ev)
		}
		p.persistWindows(dir, snapshot, marks, seq)
		if ripe != nil {
			p.runExtract(s, key, ripe)
		}
		if sweep {
			p.runSweep(s)
		}
	}()
}

// persistWindows 串行写盘并丢弃过期的快照。
func (p *Plugin) persistWindows(dir string, windows map[windowKey]*window, marks dayMarks, seq uint64) {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	if seq <= p.savedSeq {
		return // 已经写过更新的快照了，这一份是旧的
	}
	p.savedSeq = seq
	saveWindowState(dir, windows, marks)
}

// snapshotWindowsLocked 深拷贝当前缓冲，供锁外写盘使用。
// 不能把 p.windows 直接交出去：下一轮对话会继续改它，而写盘正在遍历。
func (p *Plugin) snapshotWindowsLocked() map[windowKey]*window {
	out := make(map[windowKey]*window, len(p.windows))
	for k, w := range p.windows {
		out[k] = &window{
			scope: plugin.Scope{
				Write: w.scope.Write,
				Read:  append([]string(nil), w.scope.Read...),
			},
			turns:   append([]windowTurn(nil), w.turns...),
			bytes:   w.bytes,
			lastEnd: w.lastEnd,
		}
	}
	return out
}

// accumulateLocked 把本轮并进对应的窗口，并判断是否该提炼。
// 返回非 nil 表示窗口已被取走（缓冲已清空），由调用方负责跑提炼。
func (p *Plugin) accumulateLocked(key windowKey, scope plugin.Scope, ev plugin.TurnEndEvent, s settings) *window {
	if p.windows == nil {
		p.windows = map[windowKey]*window{}
	}
	w := p.windows[key]
	if w == nil {
		w = &window{scope: scope}
		p.windows[key] = w
	}

	// 隔了足够久再开口，上一段话题多半已经结束：先把攒着的提炼掉，本轮另起一个窗口。
	// 「话题结束」比「轮数够了」更贴近记忆该落盘的时机，纯按轮数切会把两段不相干的
	// 对话并在一起送去提炼，模型反而挑不出东西。判断必须在并入本轮之前做。
	if len(w.turns) > 0 && !w.lastEnd.IsZero() && ev.StartedAt.Sub(w.lastEnd) >= idleFlushGap {
		if ripe := p.takeLocked(key, w, s); ripe != nil {
			fresh := &window{scope: scope}
			fresh.add(ev, s.turnEvery*2)
			p.windows[key] = fresh
			return ripe
		}
	}

	w.scope = scope // 可见域以最近一轮为准
	w.add(ev, s.turnEvery*2)
	if len(w.turns) < s.turnEvery {
		return nil
	}
	return p.takeLocked(key, w, s)
}

// takeLocked 在窗口够格提炼时取走它并清空缓冲，否则返回 nil（内容留着并进下个窗口）。
func (p *Plugin) takeLocked(key windowKey, w *window, s settings) *window {
	if p.extracting[key] {
		// 上一次还没跑完。后台任务堆在一起没有意义，继续攒，下次到点再说。
		return nil
	}
	if w.bytes < minWindowBytes {
		// 十轮「嗯」「好的」也会到点，不值得为它调一次模型
		return nil
	}
	delete(p.windows, key)
	return w
}

// runExtract 跑一次定期提炼。在后台 goroutine 中执行。
func (p *Plugin) runExtract(s settings, key windowKey, w *window) {
	defer func() {
		p.turnMu.Lock()
		delete(p.extracting, key)
		p.turnMu.Unlock()
	}()

	complete := p.completeFunc()
	if complete == nil || s.ctx == nil {
		return
	}
	// 用插件自己的 ctx：广播进来的那个在轮次结束时就被取消了
	ctx, cancel := context.WithTimeout(plugin.WithScope(s.ctx, w.scope), extractTimeout)
	defer cancel()

	res, err := p.extractMemories(ctx, s, complete, w.text(s.lang, turnExtractBytes))
	if err != nil {
		if ctx.Err() == nil { // 取消与超时是正常收尾，不值得记一行错误
			log.Printf("记忆提炼：本轮失败：%v", err)
		}
		return
	}
	if res.empty() {
		return
	}
	log.Printf("记忆提炼：新增/修订 %d 条（%s），另刷新 %d 条的最后提及时间",
		len(res.saved)+len(res.revised), strings.Join(res.names(s.lang), "、"), res.touched)
	p.postNotice(ctx, s.lang, key.session, res)
}

// postNotice 把这次提炼记了什么留在会话里给人看。
//
// 提炼跑在轮次收尾之后，界面那一轮的事件流已经关了，不留下痕迹的话，模型自动改了
// 什么就只有日志知道。修订尤其需要说出来：它会覆盖已有记忆，虽然覆盖前留了 .bak，
// 但没人会想到去翻。
func (p *Plugin) postNotice(ctx context.Context, lang, sessionID string, res extractResult) {
	notice := p.noticeFunc()
	if notice == nil || sessionID == "" {
		return
	}
	var parts []string
	if len(res.saved) > 0 {
		parts = append(parts, i18n.Tf(lang, "plugin.memory.notice.added", "新增 %s", quoteNames(lang, res.saved)))
	}
	if len(res.revised) > 0 {
		parts = append(parts, i18n.Tf(lang, "plugin.memory.notice.revised", "修订 %s", quoteNames(lang, res.revised)))
	}
	if len(parts) == 0 {
		return // 只刷新了提及时间，没有内容变化，不值得打扰
	}
	text := i18n.Tf(lang, "plugin.memory.notice.line", "🧠 记忆提炼：%s",
		strings.Join(parts, i18n.T(lang, "plugin.memory.notice.sep", "，")))
	if err := notice(ctx, sessionID, text); err != nil {
		log.Printf("记忆提炼：注记写入失败：%v", err)
	}
}

// quoteNames 把条目渲染成「分类/标题」的顿号列表。
func quoteNames(lang string, entries []Entry) string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, i18n.Tf(lang, "plugin.memory.notice.quoted", "「%s/%s」",
			memTypes.For(lang, e.Type), e.Name))
	}
	return strings.Join(out, i18n.T(lang, "plugin.memory.notice.name_sep", "、"))
}
