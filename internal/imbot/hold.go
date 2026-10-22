package imbot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wen/internal/availability"
	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 忙时晚点回：角色忙着、睡着时，入站消息先扣住，过一阵再作为一轮处理。
//
// 回复时间本身就是信息——一个永远秒回的人不像人。忙不忙由忙碌公告板
// （internal/availability）说了算：日程的活动、心跳的睡眠都写在那里。这里只改时序，
// 「在忙」要不要说、怎么解释回晚了，仍归模型——放行那一轮会注入一行事实（见 heldNote）。
//
// 按档位：空闲、轻忙立即处理；重忙抽空看一眼（最多扣 heavyHold）；不回扣到忙完；
// 睡着攒 WakeAfter 就算被吵醒。一律不超过 ReplyWindow（平台的被动回复窗口）。
// 命令与确认答复不扣：它们是对操作者的即时反馈。
//
// 扣着的消息落盘（held.json，0600），重启后按原来的到达时刻接着算——只放内存的话，
// 一次重启就会把对方的话悄悄吞掉。

// heavyHold 是重忙时最多扣多久：「只能简短回」的人会抽空看一眼手机。
const heavyHold = 10 * time.Minute

// DefaultWakeAfter 是睡着时攒多久消息算把人吵醒。
const DefaultWakeAfter = 15 * time.Minute

// heldMsg 是一条被扣住的消息，连同到达时刻与当时在忙什么。
type heldMsg struct {
	Msg      Message   `json:"msg"`
	Arrived  time.Time `json:"arrived"`
	Activity string    `json:"activity,omitempty"`
	Sleeping bool      `json:"sleeping,omitempty"`
}

// holder 按用户攒着被扣住的消息。状态由 mu 保护；放行由 Core 的 holdLoop 统一做。
type holder struct {
	mu    sync.Mutex
	path  string
	users map[string][]heldMsg
	kick  chan struct{} // 有变化时叫醒 holdLoop 重算
}

func newHolder(stateDir string) *holder {
	h := &holder{path: filepath.Join(stateDir, "held.json"), users: map[string][]heldMsg{}, kick: make(chan struct{}, 1)}
	if raw, err := os.ReadFile(h.path); err == nil {
		_ = json.Unmarshal(raw, &h.users) // 坏文件当没有：丢的是扣着的几条，不该让通道起不来
	}
	return h
}

func (h *holder) poke() {
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

// saveLocked 原子写回；没有扣着的消息时删掉文件。调用方须持 mu。
//
// 目录要自己建：通道初始化时并不创建 StateDir，它往往要等第一次建立会话绑定才出现，
// 而第一条消息就可能被扣住——那时目录还不存在，写盘失败而消息只在内存里，一次重启
// 就没了。
func (h *holder) saveLocked() error {
	if len(h.users) == 0 {
		if err := os.Remove(h.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(h.users)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, h.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// releaseAt 算出一批从 first 起扣着的消息该在何时放行；不需要扣时返回 now。
func (c *Core) releaseAt(first, now time.Time) time.Time {
	st, ok := availability.Current(now)
	if !ok || st.Level < availability.Heavy {
		return now
	}
	var rel time.Time
	switch {
	case st.Sleeping:
		rel = minTime(first.Add(c.cfg.WakeAfter), st.Until)
	case st.Level == availability.Heavy:
		rel = minTime(first.Add(heavyHold), st.Until)
	default:
		rel = st.Until
	}
	if c.cfg.ReplyWindow > 0 {
		rel = minTime(rel, first.Add(c.cfg.ReplyWindow))
	}
	return rel
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// holdOrPass 决定一条普通消息是扣住还是放行，扣住时返回 true。
// 该用户已有扣着的消息时一律接在后面——先到的还没回，后到的不能插队。
func (c *Core) holdOrPass(msg Message) bool {
	if isCommand(msg) {
		return false
	}
	h := c.hold
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if prev := h.users[msg.UserID]; len(prev) > 0 {
		// 前面还有扣着的，这条只能接在后面：放行的话就插了队。存不上盘也照样扣着，
		// 只是这一批扛不过重启——记一笔，让人知道
		h.users[msg.UserID] = append(prev, heldMsg{Msg: msg, Arrived: now})
		if err := h.saveLocked(); err != nil {
			c.logHoldSaveFailed(err)
		}
		h.poke()
		return true
	}
	if !c.cfg.HoldWhenBusy || !c.releaseAt(now, now).After(now) {
		return false
	}
	st, _ := availability.Current(now)
	h.users[msg.UserID] = []heldMsg{{Msg: msg, Arrived: now, Activity: st.Activity, Sleeping: st.Sleeping}}
	if err := h.saveLocked(); err != nil {
		// 存不上盘就不扣：立刻回总比扣在内存里、一重启就悄悄丢了强
		delete(h.users, msg.UserID)
		c.logHoldSaveFailed(err)
		return false
	}
	h.poke()
	i18n.Logf(c.cfg.Lang, "log.imbot.held", "%s: 角色正忙（%s），用户 %s 的消息先扣住", c.cfg.PluginName, st.Activity, msg.UserID)
	return true
}

// releaseDue 放行到点的批次，返回最近一批还要等到的时刻（没有则为零值）。
// 开关关掉之后，残留的批次（重启前扣下的）一律立即放行。
func (c *Core) releaseDue(now time.Time) time.Time {
	h := c.hold
	var (
		due  [][]heldMsg
		next time.Time
	)
	if !c.Running() { // 正在停：放出去也入不了队，留在盘上等下次启动
		return time.Time{}
	}
	h.mu.Lock()
	for user, batch := range h.users {
		rel := now
		if c.cfg.HoldWhenBusy {
			rel = c.releaseAt(batch[0].Arrived, now)
		}
		if !rel.After(now) {
			due = append(due, batch)
			delete(h.users, user)
			continue
		}
		if next.IsZero() || rel.Before(next) {
			next = rel
		}
	}
	if len(due) > 0 {
		if err := h.saveLocked(); err != nil {
			c.logHoldSaveFailed(err) // 盘上还留着这几条，重启后会再放一次
		}
	}
	h.mu.Unlock()

	ctx := c.lifeCtx(context.Background())
	for _, batch := range due {
		// 按**此刻**的白名单再认一次：扣着的这段时间里（尤其跨过一次重启）用户可能已被
		// 移出白名单，入队之后就会触发模型与工具执行，Handle 里那一道已经管不到了
		if user := batch[0].Msg.UserID; !c.allowed(user) {
			i18n.Logf(c.cfg.Lang, "log.imbot.held_dropped",
				"%s: 用户 %s 已不在白名单内，丢弃扣着的 %d 条消息", c.cfg.PluginName, user, len(batch))
			continue
		}
		c.enqueueNow(ctx, releasedMessage(batch))
	}
	return next
}

func (c *Core) logHoldSaveFailed(err error) {
	i18n.Logf(c.cfg.Lang, "log.imbot.hold_save_failed", "%s: 扣着的消息存盘失败: %v", c.cfg.PluginName, err)
}

// releasedMessage 把一批扣着的消息合成一轮，并记下「从何时起、因为什么」被扣着。
func releasedMessage(batch []heldMsg) Message {
	msgs := make([]Message, len(batch))
	for i, b := range batch {
		msgs[i] = b.Msg
	}
	out := mergeMessages(msgs)
	out.heldSince, out.heldFor, out.heldAsleep = batch[0].Arrived, batch[0].Activity, batch[0].Sleeping
	return out
}

// holdLoop 是扣留的唯一放行者：到点、公告板变化、新消息扣下时重算一遍。
func (c *Core) holdLoop(ctx context.Context) {
	defer c.wg.Done()
	for {
		next := c.releaseDue(time.Now())
		var (
			t     *time.Timer
			fired <-chan time.Time // 没有在等的批次时为 nil，永不触发
		)
		if !next.IsZero() {
			t = time.NewTimer(time.Until(next))
			fired = t.C
		}
		select {
		case <-ctx.Done():
		case <-c.hold.kick:
		case <-fired:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// ---------- 放行那一轮告诉模型「回晚了」 ----------

type heldKey struct{}

type heldInfo struct {
	since    time.Time
	activity string
	asleep   bool
}

func withHeld(ctx context.Context, msg Message) context.Context {
	if msg.heldSince.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, heldKey{}, heldInfo{since: msg.heldSince, activity: msg.heldFor, asleep: msg.heldAsleep})
}

// heldNote 是放行那一轮注入的一行事实：对方的话是多久以前发来的、那时你在做什么。
// 只给事实，道不道歉、怎么解释归模型。不到一分钟的不提——那算不上回晚了。
func heldNote(ctx context.Context, lang string) string {
	h, ok := ctx.Value(heldKey{}).(heldInfo)
	if !ok {
		return ""
	}
	ago := time.Since(h.since)
	if ago < time.Minute {
		return ""
	}
	if h.asleep {
		return prompt.Tf(lang, "prompt.imbot.held_asleep",
			"[迟到的消息]\n对方的消息是约 %s前发来的，那时你睡着了，现在才看到。", prompt.Duration(lang, ago))
	}
	return prompt.Tf(lang, "prompt.imbot.held_busy",
		"[迟到的消息]\n对方的消息是约 %s前发来的，那时你在忙「%s」，现在才看到。", prompt.Duration(lang, ago), h.activity)
}

// heldPrompt 给本通道发起的轮次注入「回晚了」那一行，别的轮次一律不出现。
func (c *Core) heldPrompt(ctx context.Context) string {
	if plugin.TurnOriginFrom(ctx) != c.cfg.PluginName {
		return ""
	}
	return heldNote(ctx, c.cfg.Lang)
}
