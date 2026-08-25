// Package imbot 是远程 IM 通道的公共骨架。
//
// 每条通道（QQ、微信、Telegram、飞书、Lark）都要做同一批事：把远端用户映射到一个
// 会话、按用户串行处理消息、认一套 /new /status /compact /help 命令、把危险操作的
// 确认请求发到远端再等回复、把后台轮次的结果推回去。这些与协议无关的部分放这里，
// 各插件只剩自己的协议层（鉴权、收发、错误码、格式降级）。
//
// 抽出来的直接理由是重复：QQ 与微信两个插件的确认代理曾是逐行相同的两份，命令层
// 约六成重合，会话绑定与去重完全一致。再加三条通道就是五份同构代码，改一处忘四处。
package imbot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"wen/internal/plugin"
)

// 默认参数。通道可在 Config 里覆盖。
const (
	DefaultQueueSize   = 8                // 每用户待处理消息队列长度，满了直接回复稍候
	DefaultTurnTimeout = 10 * time.Minute // 单轮对话的上限
)

// Sender 是每条通道自己要实现的部分：把一段文本发给某个用户。
//
// 分段、格式转换（markdown / 纯文本 / 卡片）、失败重试与日志都由实现负责——各平台
// 的长度上限与降级策略不一样，骨架不替它们决定。因此没有返回值：发不出去是通道
// 自己的事，骨架继续往下走。
type Sender interface {
	Send(ctx context.Context, userID, text, replyTo string)
}

// Message 是一条已归一化的入站消息。
type Message struct {
	UserID  string // 通道内的用户标识（QQ openid / 微信 xxx@im.wechat / Telegram chat_id / 飞书 open_id）
	DedupID string // 去重键；为空表示不去重
	// ReplyTo 是通道自定义的「回复凭据」，骨架不解释内容，原样交回 Sender：
	// QQ 是 msg_id，微信是 context_token，Telegram 是 message_id，飞书是 message_id。
	ReplyTo string
	Text    string
	// Note 是收到了正文之外的东西时给用户的提示（如「暂不支持该消息类型」）。
	// Text 为空且 Note 非空时，骨架把 Note 回过去就结束，不进队列。
	Note string
}

// Config 是构造 Core 所需的一切。字段在 New 之后不再变动——改配置走「重建 Core」，
// 因此 Core 内部读它们不需要加锁。
type Config struct {
	PluginName string // 插件名，用于日志前缀
	Sender     Sender
	StateDir   string // 会话绑定落在 <StateDir>/sessions.json
	// Lang 是这条通道对外说话用的语言（命令回执、错误提示、注入模型的引导）。
	// 由插件从 InitContext.Lang 原样带下来；空串按中文处理。
	//
	// 不按入站消息猜：同一个用户可能中英文混着发，而这条通道的回执与它所在服务
	// 的其它出口（Web UI 的状态行、日志）说的应该是同一门语言。
	Lang string

	ConfirmTimeout time.Duration
	TurnTimeout    time.Duration // 0 表示用 DefaultTurnTimeout
	QueueSize      int           // 0 表示用 DefaultQueueSize

	ShowThinking bool
	ShowTools    bool
	// PushNotices 开启后把会话注记（后台工作留下的说明，如提炼的记录）推给绑定用户。
	PushNotices bool

	// MergeWindow 是入站合并窗口：同一用户在这段时间内连发的普通消息合成一轮，
	// 每来一条重新计时，总等待封顶为窗口的 mergeCapFactor 倍。0 表示关闭，逐条成轮。
	MergeWindow time.Duration
	// HumanPace 开启后，助手的回复拆成几条、带打字间隔陆续发出，并在本通道发起的
	// 轮次里注入「像发消息那样说话」的引导（见 pace.go）。命令回执、错误提示、
	// 确认请求与过程通知不受影响。
	HumanPace bool
	// sleep 是分条发送之间的等待，留空用真实计时；测试注入以免真等。
	sleep func(ctx context.Context, d time.Duration)

	// Allow 判定用户是否放行。为 nil 表示一律拒绝——白名单为空时拒绝所有人是各通道
	// 一致的既定行为，把它做成默认值可以避免某条通道忘了实现就变成对全网开放。
	Allow func(userID string) bool
	// OnAccepted 在一条消息通过去重与准入之后调用，供通道做自己的记账
	// （微信在这里记住该用户最近的 context_token，后台推送要用）。可为 nil。
	OnAccepted func(msg Message)
	// Typing 在跑一轮对话前后调用，供通道发「正在输入」状态。可为 nil。
	Typing func(ctx context.Context, msg Message, active bool)
	// Push 是主动推送：没有「本轮入站消息」可回带凭据时把文本发给某个用户。
	// 返回是否真的交给了平台——微信必须回带该用户最近一次入站消息的
	// context_token，对方从没说过话时就推不出去，调用方要据此知道这段话没送达。
	// 为 nil 表示本通道不接受转投。
	Push func(ctx context.Context, userID, text string) bool
	// Notice 往会话里留一行只给人看的说明（不进模型上下文），用于报告转投失败
	// 这类「事情发生在轮次之外、没有回复渠道」的情况。可为 nil。
	Notice plugin.NoticeFunc

	RunTurn    plugin.RunTurnFunc
	NewSession plugin.NewSessionFunc
	Compact    plugin.CompactFunc
	Status     plugin.StatusFunc
	Sessions   plugin.SessionQuery
}

// Core 是通道无关的那一半：分发、命令、会话绑定、确认代理、串行 worker。
type Core struct {
	cfg     Config
	binding *Binding
	dedup   *Deduper
	broker  *confirmBroker
	merge   *merger // 入站合并窗口；MergeWindow 为 0 时为 nil

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	workers map[string]chan Message
	wg      sync.WaitGroup
}

// New 构造 Core 并加载已有的会话绑定。
func New(cfg Config) (*Core, error) {
	if cfg.Sender == nil {
		return nil, fmt.Errorf("imbot: 需要 Sender")
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("imbot: 需要持久化目录")
	}
	if cfg.RunTurn == nil || cfg.NewSession == nil {
		return nil, fmt.Errorf("当前环境不支持插件发起对话轮次")
	}
	if cfg.Sessions == nil {
		return nil, fmt.Errorf("当前环境不支持会话查询")
	}
	binding, err := loadBinding(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("加载会话映射失败: %w", err)
	}
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = DefaultTurnTimeout
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	c := &Core{
		cfg:     cfg,
		binding: binding,
		dedup:   NewDeduper(),
		broker:  newConfirmBroker(),
		workers: map[string]chan Message{},
	}
	if cfg.MergeWindow > 0 {
		// 到期冲刷发生在计时器 goroutine 上，没有入站的 ctx 可用，拿 Core 自己的生命周期 ctx
		c.merge = newMerger(cfg.MergeWindow, func(msgs []Message) {
			c.enqueue(c.lifeCtx(context.Background()), mergeMessages(msgs))
		})
	}
	return c, nil
}

// Start 记下生命周期 ctx。worker 是惰性启动的，这里不起 goroutine。
func (c *Core) Start(ctx context.Context) {
	c.mu.Lock()
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()
	registerLive(c) // 登记为「活着的通道」，别的通道才能把回复转投过来
}

// Stop 停掉全部 worker 并等它们退出。可重复调用。
func (c *Core) Stop() {
	unregisterLive(c)
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if c.merge != nil {
		c.merge.stop()
	}
	if cancel != nil {
		cancel()
		c.wg.Wait()
	}
}

// Running 报告 Core 是否处于启动状态，供通道在推送前自检。
func (c *Core) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancel != nil
}

// UsersFor 反查绑定到某会话的全部用户：后台轮次落在该会话上时要推给他们。
func (c *Core) UsersFor(sessionID string) []string { return c.binding.UsersFor(sessionID) }

// SessionFor 返回该用户的当前会话，没有（或已被删除）时新建并落映射。
func (c *Core) SessionFor(userID string) (string, error) {
	// 装了分通道路由时，组内共用一个当前会话，本通道的绑定与它不一致就加入它。
	// 不这么做的话，你在另一条通道上打的字会落进那条通道自己的会话，两个人格就此
	// 分家，而回表时的自动接管又会把人从原会话拖走（见 group.go）。
	if sid := groupAnchor(); sid != "" && c.cfg.Sessions.Exists(sid) {
		if c.binding.Get(userID) != sid {
			if err := c.binding.Set(userID, sid); err != nil {
				return "", err
			}
			log.Printf("%s: 用户 %s 加入分通道的当前会话 %s", c.cfg.PluginName, userID, sid)
		}
		return sid, nil
	}
	if sid := c.binding.Get(userID); sid != "" {
		if c.cfg.Sessions.Exists(sid) {
			setGroupAnchor(sid)
			return sid, nil
		}
		// 会话文件已被删除，重建
	}
	sid, err := c.cfg.NewSession()
	if err != nil {
		return "", err
	}
	if err := c.binding.Set(userID, sid); err != nil {
		return "", err
	}
	setGroupAnchor(sid)
	return sid, nil
}

// Handle 是入站消息的分发层：去重 → 准入 → /apply /deny 直投确认代理 → 其余进队列。
//
// /apply 与 /deny 不能进队列：worker 此刻正阻塞在等确认上，排队就是自锁。
func (c *Core) Handle(ctx context.Context, msg Message) {
	if msg.UserID == "" {
		return
	}
	if c.dedup.IsDuplicate(msg.DedupID) {
		return
	}
	if !c.allowed(msg.UserID) {
		log.Printf("%s: 拒绝了白名单之外的用户 %s（如需放行请加入白名单）", c.cfg.PluginName, msg.UserID)
		return
	}
	if c.cfg.OnAccepted != nil {
		c.cfg.OnAccepted(msg)
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		if msg.Note != "" {
			c.send(ctx, msg, msg.Note)
		}
		return
	}

	// 命令不参与合并、立即处理；窗口里已经攒着的先作为一轮入队，顺序才与到达一致
	if c.merge != nil && strings.HasPrefix(text, "/") {
		c.merge.flushUser(msg.UserID)
	}

	switch text {
	case "/apply", "/deny":
		approved := text == "/apply"
		ch, ok := c.broker.take(msg.UserID)
		if !ok {
			c.send(ctx, msg, "当前没有等待确认的操作。")
			return
		}
		// 先回执、再投答复：反过来的话，被解开的那一轮可能抢在回执前面把结果发出去，
		// 用户会先看到执行结果再看到「已允许」
		if approved {
			c.send(ctx, msg, "✅ 已允许，继续执行。")
		} else {
			c.send(ctx, msg, "🚫 已拒绝该操作。")
		}
		ch <- approved
		return
	}

	if c.merge != nil && !strings.HasPrefix(text, "/") {
		c.merge.add(msg)
		return
	}
	c.enqueue(ctx, msg)
}

func (c *Core) allowed(userID string) bool {
	if c.cfg.Allow == nil {
		return false
	}
	return c.cfg.Allow(userID)
}

func (c *Core) send(ctx context.Context, msg Message, text string) {
	c.cfg.Sender.Send(ctx, msg.UserID, text, msg.ReplyTo)
}

// enqueue 把消息放进该用户的串行队列，worker 不存在时惰性启动。队列满说明上一条
// 还在处理，直接告知稍候而不是无限堆积。
func (c *Core) enqueue(ctx context.Context, msg Message) {
	c.mu.Lock()
	if c.cancel == nil { // 尚未 Start 或已 Stop
		c.mu.Unlock()
		return
	}
	q, ok := c.workers[msg.UserID]
	if !ok {
		q = make(chan Message, c.cfg.QueueSize)
		c.workers[msg.UserID] = q
		c.wg.Add(1)
		go c.worker(c.ctx, q)
	}
	c.mu.Unlock()

	select {
	case q <- msg:
	default:
		c.send(ctx, msg, "上一条消息还在处理中，请稍候再发。")
	}
}

// worker 串行处理单个用户的消息。开着合并窗口时，一次把队里已有的都取出来：
// 上一轮跑着的时候攒下的几条，出队时同样合成一轮（命令仍各自独立）。
func (c *Core) worker(ctx context.Context, q chan Message) {
	defer c.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-q:
			if c.merge == nil {
				c.process(ctx, msg)
				continue
			}
			batch := []Message{msg}
		drain:
			for {
				select {
				case m := <-q:
					batch = append(batch, m)
				default:
					break drain
				}
			}
			c.processBatch(ctx, batch)
		}
	}
}

// PushNotice 把一条会话注记推给绑定该会话的用户（「推送后台通知」开关，经 Push 通道，
// 推送本身不产生任何会话记录）。只推**插件后台工作**留下的注记（来源非空且不是
// IM 通道），其余一律不推：
//   - 来源为空的（前台轮次里写下的，如失败转译留下的报错原文）：那轮对话发生在
//     别的界面上，把它的旁注推到 IM 只会是一条没头没尾的内部信息——失败转译藏起
//     报错原文正是为了沉浸感，从注记这条边路推出去等于前门挡住后门漏；
//   - 带可见域标签的：它属于某一面人格，而路由只认「会话→通道」不认标签，拿不准
//     归属就不送——宁可少推，不把一面人格的动静泄漏到另一面的窗口里；
//   - IM 通道自己发的（如转投失败的说明）：那是给操作者看的旁注，推给用户只会暴露
//     另一条通道的存在；
//   - 装了分通道路由且本会话不归本通道服务的：注记跟着回复走同一条出口。
//
// 与转投用的 pushTo 不同，这里只推给**已经绑定**该会话的用户，刻意不做「自动把
// 唯一已知用户接入会话」：注记是旁注，不值得为它建立会话绑定。
func (c *Core) PushNotice(ev plugin.NoticeEvent) {
	if !c.cfg.PushNotices || c.cfg.Push == nil {
		return
	}
	if strings.TrimSpace(ev.Text) == "" || ev.Tag != "" || ev.Origin == "" || IsChannel(ev.Origin) {
		return
	}
	if !ServedBy(c.cfg.PluginName, ev.SessionID) {
		return
	}
	users := c.binding.UsersFor(ev.SessionID)
	if len(users) == 0 {
		return
	}
	// 广播发生在注记写入方的路径上，发送工作放 goroutine；用 Core 的生命周期 ctx，
	// 写入方的 ctx 可能在广播返回后立即被取消。
	c.mu.Lock()
	if c.cancel == nil {
		c.mu.Unlock()
		return
	}
	ctx := c.ctx
	c.wg.Add(len(users))
	c.mu.Unlock()
	for _, userID := range users {
		go func(userID string) {
			defer c.wg.Done()
			if !c.cfg.Push(ctx, userID, ev.Text) {
				log.Printf("%s: 注记推送未送达 %s", c.cfg.PluginName, userID)
			}
		}(userID)
	}
}

// Bind 把用户改绑到指定会话（/new 之外，通道自己也可能需要，如测试与迁移）。
func (c *Core) Bind(userID, sessionID string) error { return c.binding.Set(userID, sessionID) }

// BoundSession 返回该用户当前绑定的会话 ID，未绑定时为空串。
// 与 SessionFor 不同，它不会新建会话，只是查一眼。
func (c *Core) BoundSession(userID string) string { return c.binding.Get(userID) }
