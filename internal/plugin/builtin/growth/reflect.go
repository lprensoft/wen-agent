package growth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// OnTurnEnd 是反思的触发点：新的一天里第一轮对话（真人的，或心跳的一拍）结束后，在
// 同一个会话上起一轮【成长·反思】。照 agenda 规划当天的做法，不另设固定时刻——放在
// 早上而不是夜里还有一层用意：心情、唤起这类状态过了一夜已经回落，正好躲开
// 「情绪起伏时不改自己」。
//
// 前一次反思之后没有真人来聊过（Dirty 为假）就不反思：没有新东西可想，不为它付一轮
// 模型调用。Dirty 在真人轮次收尾时置上；触发反思的那一轮若是真人的，它算下一次的
// 材料（反思跑完时 Dirty 按它重新置上），于是最多每天一次，而一整天没人来过的日子不会有。
//
// 本方法在轮次收尾的同步路径上，只读一次文件、必要时写一次，然后起 goroutine。
func (p *Plugin) OnTurnEnd(ctx context.Context, ev plugin.TurnEndEvent) {
	if ev.Origin == p.Name() {
		return // 反思自己的轮次不触发
	}
	s := p.snapshot()
	tag := plugin.ScopeFrom(ctx).Write
	store := p.storeFor(tag)
	if store == nil {
		return
	}
	today := logicalDay(p.now())
	st, err := store.Load()
	if err != nil {
		i18n.Logf(s.lang, "log.growth.read_failed", "growth: 读取成长记录失败: %v", err)
		return
	}
	due := s.reflect && s.runTurn != nil && st.Dirty && st.LastReflect != today && st.LastReflect != ""
	if st.LastReflect == "" {
		// 第一次见到这个域：从今天起算，不为启用之前的日子补一次反思
		_, _ = store.Update(func(st *State) (bool, []LogEntry, error) {
			if st.LastReflect != "" {
				return false, nil, nil
			}
			st.LastReflect = today
			return true, nil, nil
		})
	}
	if ev.Interactive && !st.Dirty {
		_, _ = store.Update(func(st *State) (bool, []LogEntry, error) {
			if st.Dirty {
				return false, nil, nil
			}
			st.Dirty = true
			return true, nil, nil
		})
	}
	if !due {
		return
	}
	p.mu.Lock()
	if p.cancel == nil || p.reflecting[tag] {
		p.mu.Unlock()
		return
	}
	p.reflecting[tag] = true
	p.changed[tag] = nil
	p.wg.Add(1)
	rctx := p.ctx
	p.mu.Unlock()
	go p.runReflect(rctx, tag, ev.SessionID, today, ev.Interactive)
}

// runReflect 跑一次反思：等触发那轮彻底收尾，再以【成长·反思】为一次性输入起轮次。
// 会话忙限次重试；跑完（不论改没改）都记下今天反思过，失败也记——明天再说，不在
// 同一天反复烧调用。interactive 表示触发那一轮是真人的：那一轮算下一次的材料，
// 反思跑完时 Dirty 按它重新置上。
func (p *Plugin) runReflect(ctx context.Context, tag, sid, today string, interactive bool) {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		delete(p.reflecting, tag)
		delete(p.changed, tag)
		p.mu.Unlock()
	}()
	select {
	case <-time.After(p.settle):
	case <-ctx.Done():
		return
	}
	s := p.snapshot()
	store := p.storeFor(tag)
	if store == nil || s.runTurn == nil {
		return
	}
	st, err := store.Load()
	if err != nil {
		i18n.Logf(s.lang, "log.growth.read_failed", "growth: 读取成长记录失败: %v", err)
		return
	}
	input := buildReflectPrompt(s, st, p.now())

	for attempt := 0; ; attempt++ {
		tctx, cancel := context.WithTimeout(ctx, p.runTimeout)
		tctx = plugin.WithEphemeralInput(tctx)
		_, err = s.runTurn(tctx, sid, input)
		cancel()
		if errors.Is(err, plugin.ErrSessionBusy) && attempt+1 < p.busyMax {
			select {
			case <-time.After(p.busyRetry):
				continue
			case <-ctx.Done():
				return
			}
		}
		break
	}
	if ctx.Err() != nil {
		return // 插件停了：不记作反思过，下次启动后再来
	}
	if err != nil {
		i18n.Logf(s.lang, "log.growth.reflect_failed", "growth: 反思轮次失败（明天再试）: %v", err)
	}
	_, _ = store.Update(func(st *State) (bool, []LogEntry, error) {
		st.LastReflect = today
		st.Dirty = interactive
		return true, nil, nil
	})

	p.mu.Lock()
	lines := p.changed[tag]
	p.mu.Unlock()
	if len(lines) > 0 && s.notice != nil {
		text := i18n.Tf(s.lang, "plugin.growth.notice.summary", "🌱 成长：%s",
			strings.Join(lines, i18n.T(s.lang, "plugin.growth.notice.sep", "；")))
		if err := s.notice(plugin.WithScope(ctx, plugin.Scope{Write: tag}), sid, text); err != nil {
			i18n.Logf(s.lang, "log.growth.notice_failed", "growth: 写会话注记失败: %v", err)
		}
	}
}

// buildReflectPrompt 拼反思轮次的一次性输入：时刻、现有的成长（带编号，modify 与
// let_go 要点名）、今天的限额与待批准的那一条。
func buildReflectPrompt(s settings, st State, now time.Time) string {
	lang := s.lang
	var cur strings.Builder
	if len(st.Core) > 0 {
		cur.WriteString(prompt.T(lang, "prompt.growth.reflect.core", "已经定下来的大变化："))
		for _, e := range st.Core {
			fmt.Fprintf(&cur, "\n- %s %s", e.ID, e.Text)
		}
		cur.WriteString("\n")
	}
	cur.WriteString(prompt.Tf(lang, "prompt.growth.reflect.details", "小变化（%d/%d 条）：", len(st.Details), s.maxDetails))
	if len(st.Details) == 0 {
		cur.WriteString(prompt.T(lang, "prompt.growth.reflect.none", "还没有"))
	}
	for _, e := range st.Details {
		fmt.Fprintf(&cur, "\n- %s %s", e.ID, e.Text)
	}

	used := 0
	if st.Quota.Date == logicalDay(now) {
		used = st.Quota.Used
	}
	pending := prompt.T(lang, "prompt.growth.reflect.no_pending", "现在没有在等批准的提议。")
	if st.Pending != nil {
		pending = prompt.Tf(lang, "prompt.growth.reflect.pending",
			"已经有一条在等对方批准（「%s」），今天不要再提。", st.Pending.Text)
	}
	return prompt.Tf(lang, "prompt.growth.reflect", reflectZH,
		now.Format("2006-01-02 15:04"), cur.String(), max(s.dailyChanges-used, 0), pending)
}

// reflectZH 是反思轮次的一次性输入。
//
// 三个问题只问「角色自己」：对方的习惯与爱好归记忆（会自动提炼），两人之间的默契归
// 关系，这里不重复。「说不清为什么这还是你的，就不是成长」与工具的必填项同一件事：
// 照镜子。结尾按约定写明不要再调工具、输出空回复——这一轮要先调工具再收尾，不写清楚
// 的话模型最不可能产出的恰恰是空回复，它会改去反复调工具。
const reflectZH = `【成长·反思】现在是 %s。回头看看最近这些日子（[最近几天]、记得的事与刚才的对话），想想你自己有没有长出点什么：
1. 有没有新冒出来的喜欢、讨厌、习惯、口头禅，或者生活里的小细节？
2. 有没有哪段经历让你在某件事上的态度、倾向变了？
3. 性格、待人或说话的方式，有没有一种持续了好些天的变化？
你现在的成长：
%s
规则：
- 根在你的角色设定里：这里只能在它上面长，不能推翻它。和设定冲突的变化不要记。
- 只记你自己身上的。对方的习惯与爱好、你们之间的默契不在这里记。
- 小的变化（1、2）用 grow_detail 自己记，今天还能改 %d 条；满了就先放下或换掉一条旧的。
- 大的变化（3）用 propose_core_change 提一条，等对方批准才算数。%s
- 每条写清依据（哪天、哪件事）和为什么这还是你。说不清为什么这还是你的，就不是成长，不记。
- 最近情绪起伏大（吵了架、刚亲密过、被冷落、很累）的话，今天什么都别改，等平静下来。
- 没有值得记的就什么都不做，大多数日子都是这样。
做完或决定不做就结束：不要再调用别的工具，不必对对方说话，输出空回复即可。`
