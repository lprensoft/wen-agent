package imbot

import (
	"context"
	"strings"
	"sync"
	"time"
)

// 入站合并窗口：同一个人连发的几条消息合成一轮。
//
// 真人聊天常把一句话拆成三条发，而机器原来的做法是每条起一轮、每条都回——对方
// 还没说完就插嘴，还一连插三次。窗口的做法是收到一条普通消息后先等一会儿，
// 期间同一用户的后续消息并进来，直到安静了一个窗口（或总等待触顶）再作为一轮
// 入队。命令不参与：它们是即时操作，攒着只会让人以为没反应。
//
// 窗口只管「还没入队」的消息。已经排在队里等 worker 的那些，在出队时由
// processBatch 再合一次——轮次跑着的时候攒下的几条，同样该一起回。

// mergeCapFactor 是总等待时长相对窗口的倍数：对方一直在打字时不能无限等下去。
const mergeCapFactor = 3

// pendingBatch 是某个用户在窗口里攒着的消息。
type pendingBatch struct {
	msgs     []Message
	timer    *time.Timer
	deadline time.Time // 首条到达时刻 + 封顶时长，过了就不再重新计时
}

// merger 按用户维护合并窗口。timer 回调与 Handle 并发，状态由 mu 保护。
type merger struct {
	mu      sync.Mutex
	window  time.Duration
	pending map[string]*pendingBatch
	flush   func(msgs []Message) // 在锁外调用
}

func newMerger(window time.Duration, flush func([]Message)) *merger {
	return &merger{window: window, pending: map[string]*pendingBatch{}, flush: flush}
}

// add 把一条消息放进该用户的窗口：首条开窗，后续重新计时，总等待触顶时当场冲刷。
func (m *merger) add(msg Message) {
	now := time.Now()
	m.mu.Lock()
	p, ok := m.pending[msg.UserID]
	if !ok {
		p = &pendingBatch{deadline: now.Add(mergeCapFactor * m.window)}
		m.pending[msg.UserID] = p
	} else if p.timer != nil {
		p.timer.Stop()
	}
	p.msgs = append(p.msgs, msg)
	wait := m.window
	if rem := p.deadline.Sub(now); rem < wait {
		wait = rem
	}
	if wait <= 0 {
		delete(m.pending, msg.UserID)
		m.mu.Unlock()
		m.flush(p.msgs)
		return
	}
	// 回调里只认「仍是这一批」：Stop 与到期可能擦肩而过，旧回调不得冲掉新攒的一批
	p.timer = time.AfterFunc(wait, func() { m.fire(msg.UserID, p) })
	m.mu.Unlock()
}

// fire 是到期回调：这一批还挂着才冲刷。
func (m *merger) fire(userID string, p *pendingBatch) {
	m.mu.Lock()
	if m.pending[userID] != p {
		m.mu.Unlock()
		return
	}
	delete(m.pending, userID)
	m.mu.Unlock()
	m.flush(p.msgs)
}

// flushUser 立即冲刷该用户攒着的消息（命令到达时用：先把已攒的作为一轮入队，再处理命令）。
func (m *merger) flushUser(userID string) {
	m.mu.Lock()
	p, ok := m.pending[userID]
	if ok {
		delete(m.pending, userID)
		if p.timer != nil {
			p.timer.Stop()
		}
	}
	m.mu.Unlock()
	if ok {
		m.flush(p.msgs)
	}
}

// stop 停掉全部计时器并丢弃攒着的消息。Core 停下之后入队本来就会被拒绝，
// 留着计时器只是让回调撞上一个已经停掉的 Core。
func (m *merger) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for user, p := range m.pending {
		if p.timer != nil {
			p.timer.Stop()
		}
		delete(m.pending, user)
	}
}

// mergeMessages 把几条消息合成一条：正文按到达顺序用换行连接，回复凭据取最后一条
// ——平台的被动回复窗口、context_token 这类凭据以最新的那条为准。
func mergeMessages(msgs []Message) Message {
	if len(msgs) == 1 {
		return msgs[0]
	}
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if t := strings.TrimSpace(m.Text); t != "" {
			texts = append(texts, t)
		}
	}
	last := msgs[len(msgs)-1]
	out := Message{
		UserID:  last.UserID,
		DedupID: last.DedupID,
		ReplyTo: last.ReplyTo,
		Text:    strings.Join(texts, "\n"),
	}
	// 被扣过的消息与排在它后面的几条并成一轮时，「从何时起没回」按最早的那条算
	for _, m := range msgs {
		if !m.heldSince.IsZero() && (out.heldSince.IsZero() || m.heldSince.Before(out.heldSince)) {
			out.heldSince, out.heldFor, out.heldAsleep = m.heldSince, m.heldFor, m.heldAsleep
		}
	}
	return out
}

// isCommand 判定一条消息是不是命令：以 / 开头的都算，未知命令也走命令路径（立即回一句未知）。
func isCommand(msg Message) bool {
	return strings.HasPrefix(strings.TrimSpace(msg.Text), "/")
}

// processBatch 处理从队列里一次取出的几条消息：相邻的普通消息合成一轮，命令各自
// 独立处理，先后顺序与到达顺序一致。
func (c *Core) processBatch(ctx context.Context, batch []Message) {
	var chat []Message
	flushChat := func() {
		if len(chat) > 0 {
			c.process(ctx, mergeMessages(chat))
			chat = nil
		}
	}
	for _, m := range batch {
		if isCommand(m) {
			flushChat()
			c.process(ctx, m)
			continue
		}
		chat = append(chat, m)
	}
	flushChat()
}
