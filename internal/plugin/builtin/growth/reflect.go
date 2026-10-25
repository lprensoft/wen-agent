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
// 模型调用。Dirty 在真人轮次收尾时置上；反思收尾时只消费它开始之前的那些（见 runReflect），
// 于是最多每天一次，而一整天没人来过的日子不会有。
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
	now := p.now()
	today := logicalDay(now)
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
	if ev.Interactive {
		// 只在内存里记一个时刻（每轮真人对话都写盘不值得）：反思收尾时凭它判断
		// 「我开始之后有没有人来过」，见 runReflect
		p.mu.Lock()
		p.lastActive[tag] = now
		p.mu.Unlock()
		if !st.Dirty {
			_, _ = store.Update(func(st *State) (bool, []LogEntry, error) {
				if st.Dirty {
					return false, nil, nil
				}
				st.Dirty = true
				return true, nil, nil
			})
		}
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
	p.wg.Add(1)
	rctx := p.ctx
	p.mu.Unlock()
	go p.runReflect(rctx, tag, ev.SessionID, today, now)
}

// runReflect 跑一次反思：等触发那轮彻底收尾，再以【成长·反思】为一次性输入起轮次。
//
// **反思落在哪个人格上，由那一轮自己认领，不由这里决定。** 这里记下的 tag 只是触发时
// 的人格；等待与忙碌重试期间对方可能已经切了人格，而 Agent 在会话锁内按会话当时的
// 状态重新裁决。所以成长清单不拼进输入（那会把触发时那个人格的私有清单交给另一个
// 人格），而是由 TurnPrompt 在裁决之后按真正的可见域给出，并在那里认领（claim）；
// 工具只接受已认领的域。收尾时按认领的那个域记账；落到了别的人格上，触发时那个域
// 今天就不算反思过，等它自己的下一轮再来。
//
// 会话忙限次重试；跑完（不论改没改）都记下认领的那个域今天反思过，失败也记——明天
// 再说，不在同一天反复烧调用。startedAt 是触发时刻：收尾时只消费它之前的真人对话，
// 之后来的（包括另一个会话里的）留给下一次。
func (p *Plugin) runReflect(ctx context.Context, tag, sid, today string, startedAt time.Time) {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		delete(p.reflecting, tag)
		if claimed, ok := p.claims[sid]; ok {
			delete(p.claimed, claimed)
			delete(p.changed, claimed)
		}
		delete(p.claims, sid)
		p.mu.Unlock()
	}()
	select {
	case <-time.After(p.settle):
	case <-ctx.Done():
		return
	}
	s := p.snapshot()
	if s.runTurn == nil {
		return
	}
	input := prompt.Tf(s.lang, "prompt.growth.reflect", reflectZH, p.now().Format("2006-01-02 15:04"))

	var err error
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

	p.mu.Lock()
	actual, claimed := p.claims[sid]
	lines := p.changed[actual]
	p.mu.Unlock()
	if !claimed {
		if errors.Is(err, plugin.ErrSessionBusy) {
			actual, claimed = tag, true // 一直忙到放弃：照样记作今天反思过，明天再说
		} else {
			if err == nil {
				i18n.Logf(s.lang, "log.growth.reflect_unclaimed",
					"growth: 反思轮次没有落在要反思的人格上（会话已切换人格或今天已反思过），等那个人格的下一轮再来")
			}
			return
		}
	}
	store := p.storeFor(actual)
	if store == nil {
		return
	}
	p.mu.Lock()
	active := p.lastActive[actual]
	p.mu.Unlock()
	_, _ = store.Update(func(st *State) (bool, []LogEntry, error) {
		st.LastReflect = today
		// 只消费反思开始之前的真人对话：开始之后有人来过（触发那一轮若是真人的，它的
		// 时刻就是 startedAt，同样算），Dirty 留着给下一次
		st.Dirty = !active.IsZero() && !active.Before(startedAt)
		return true, nil, nil
	})

	if len(lines) > 0 && s.notice != nil {
		text := i18n.Tf(s.lang, "plugin.growth.notice.summary", "🌱 成长：%s",
			strings.Join(lines, i18n.T(s.lang, "plugin.growth.notice.sep", "；")))
		if err := s.notice(plugin.WithScope(ctx, plugin.Scope{Write: actual}), sid, text); err != nil {
			i18n.Logf(s.lang, "log.growth.notice_failed", "growth: 写会话注记失败: %v", err)
		}
	}
}

// reflectionPrompt 是反思轮次里的 TurnPrompt：在 Agent 按会话当时的状态裁决了人格
// 之后调用，于是这里的可见域就是这一轮真正落在的那个人格。认领它，并给出它自己的
// 成长清单；它今天已经反思过（另一轮先到了），就不认领，只叫这一轮什么都别做。
func (p *Plugin) reflectionPrompt(ctx context.Context, sid string) (string, error) {
	s := p.snapshot()
	tag := plugin.ScopeFrom(ctx).Write
	store := p.storeFor(tag)
	if store == nil {
		return "", nil
	}
	st, err := store.Load()
	if err != nil {
		return "", err
	}
	now := p.now()
	p.mu.Lock()
	taken := p.claimed[tag]
	if st.LastReflect != logicalDay(now) && !taken {
		p.claims[sid] = tag
		p.claimed[tag] = true
		p.changed[tag] = nil
	}
	ok := p.claims[sid] == tag && p.claimed[tag]
	p.mu.Unlock()
	if !ok {
		return prompt.T(s.lang, "prompt.growth.listing.done", listingDoneZH), nil
	}
	return buildListing(s, st, now), nil
}

// buildListing 给出这个人格现在的成长：带编号（modify 与 let_go 要点名）、今天的
// 限额与待批准的那一条。
func buildListing(s settings, st State, now time.Time) string {
	lang := s.lang
	var b strings.Builder
	b.WriteString(prompt.T(lang, "prompt.growth.listing", "[你现在的成长]"))
	if len(st.Core) > 0 {
		b.WriteString("\n" + prompt.T(lang, "prompt.growth.listing.core", "已经定下来的大变化："))
		for _, e := range st.Core {
			fmt.Fprintf(&b, "\n- %s %s", e.ID, e.Text)
		}
	}
	b.WriteString("\n" + prompt.Tf(lang, "prompt.growth.listing.details", "小变化（%d/%d 条）：", len(st.Details), s.maxDetails))
	if len(st.Details) == 0 {
		b.WriteString(prompt.T(lang, "prompt.growth.listing.none", "还没有"))
	}
	for _, e := range st.Details {
		fmt.Fprintf(&b, "\n- %s %s", e.ID, e.Text)
	}
	used := 0
	if st.Quota.Date == logicalDay(now) {
		used = st.Quota.Used
	}
	b.WriteString("\n" + prompt.Tf(lang, "prompt.growth.listing.quota", "今天还能改 %d 条小变化。", max(s.dailyChanges-used, 0)))
	if st.Pending != nil {
		b.WriteString("\n" + prompt.Tf(lang, "prompt.growth.listing.pending",
			"已经有一条在等对方批准（「%s」），今天不要再提。", st.Pending.Text))
	} else {
		b.WriteString("\n" + prompt.T(lang, "prompt.growth.listing.no_pending", "现在没有在等批准的提议。"))
	}
	return b.String()
}

// listingDoneZH 是这一轮没能认领时给的话：这个人格今天已经反思过了。
const listingDoneZH = `[你现在的成长]
今天已经回头看过了，这一轮什么都不用做：不要调用工具，不必对对方说话，输出空回复即可。`

// reflectZH 是反思轮次的一次性输入。成长清单不在这里（见 runReflect：它要按这一轮
// 真正落在的人格给出），只引用它的块名。
//
// 三个问题只问「角色自己」：对方的习惯与爱好归记忆（会自动提炼），两人之间的默契归
// 关系，这里不重复。「说不清为什么这还是你的，就不是成长」与工具的必填项同一件事：
// 照镜子。结尾按约定写明不要再调工具、输出空回复——这一轮要先调工具再收尾，不写清楚
// 的话模型最不可能产出的恰恰是空回复，它会改去反复调工具。
const reflectZH = `【成长·反思】现在是 %s。回头看看最近这些日子（[最近几天]、记得的事与刚才的对话），想想你自己有没有长出点什么：
1. 有没有新冒出来的喜欢、讨厌、习惯、口头禅，或者生活里的小细节？
2. 有没有哪段经历让你在某件事上的态度、倾向变了？
3. 性格、待人或说话的方式，有没有一种持续了好些天的变化？
你现在有哪些成长、今天还能改几条，见 [你现在的成长]。
规则：
- 根在你的角色设定里：这里只能在它上面长，不能推翻它。和设定冲突的变化不要记。
- 只记你自己身上的。对方的习惯与爱好、你们之间的默契不在这里记。
- 小的变化（1、2）用 grow_detail 自己记；满了就先放下或换掉一条旧的。
- 大的变化（3）用 propose_core_change 提一条，等对方批准才算数；已经有一条在等时不要再提。
- 每条写清依据（哪天、哪件事）和为什么这还是你。说不清为什么这还是你的，就不是成长，不记。
- 最近情绪起伏大（吵了架、刚亲密过、被冷落、很累）的话，今天什么都别改，等平静下来。
- 没有值得记的就什么都不做，大多数日子都是这样。
做完或决定不做就结束：不要再调用别的工具，不必对对方说话，输出空回复即可。`
