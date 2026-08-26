package imbot

import (
	"context"
	"log"
	"sort"
	"sync"

	"wen/internal/i18n"
)

// 通道路由：让一轮对话的回复落到「另一条」通道上。
//
// 起因是表里人格想各占一条通道——在 QQ 上跟表人格聊，说出暗号后由里人格接手，
// 回复落到微信。出口跟着人格走，与这一轮从哪条通道进来无关。
//
// 这里只认通道名。「表人格 / 里人格」是 dual_persona 的词汇，路由不知道它，
// 也不知道除人格之外还能拿什么当依据——它只问一句「这个会话的回复该发到哪条
// 通道」，由安装 Router 的那个插件回答。没有插件安装 Router 时（默认情形）
// Target 恒为空串，投递路径与没有这个文件时逐字节一致。
//
// 包级可变状态的理由：插件在进程内是单实例，而路由天然是跨插件的约定——发起方
// 是某条通道的 Core，答题的是另一个插件，接收方是第三条通道的 Core，三者之间
// 没有现成的引用。核心不参与（它不知道 IM 这回事），所以约定落在 IM 这一侧的
// 公共骨架里。

var (
	regMu     sync.RWMutex
	declared  []ChannelInfo // 按声明顺序，Live 在 Channels() 里现算
	liveCores = map[string]*Core{}
	router    Router
)

// ChannelInfo 描述一条通道。Live 表示它此刻是否启用并已启动。
type ChannelInfo struct {
	Name  string // 插件名，如 "qq_bot"
	Label string // 面向人的名字，如 "QQ"
	Live  bool
}

// Router 回答「这个会话的回复该发到哪条通道」。空串表示不指定，按原路回复。
// 在 Core 的 worker goroutine 上被调用，实现必须快速返回且不得反向调用 imbot。
type Router func(sessionID string) string

// Declare 声明一条通道。各通道插件在 New() 里调用一次，与是否启用无关。
//
// 声明与启用刻意分开：候选列表要给下拉框用，而单选框的取值一旦不在候选里，
// 整份插件配置就保存不了（plugin.NormalizeConfig 对 select 型的校验）。候选
// 随启用状态增删，会让「临时关掉一条通道」变成「另一个插件的配置忽然非法」。
// 启用与否由 Channels() 返回的 Live 字段表达，标注在文案里即可。
func Declare(name, label string) {
	if name == "" {
		return
	}
	regMu.Lock()
	defer regMu.Unlock()
	for _, d := range declared {
		if d.Name == name {
			return // 幂等：New() 可能被调用多次（如测试）
		}
	}
	declared = append(declared, ChannelInfo{Name: name, Label: label})
}

// Channels 返回全部已声明的通道，按声明顺序，Live 为实时状态。
func Channels() []ChannelInfo {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]ChannelInfo, 0, len(declared))
	for _, d := range declared {
		d.Live = liveCores[d.Name] != nil
		out = append(out, d)
	}
	return out
}

// IsChannel 报告某个名字是否是一条已声明的通道。
// 用于判断一轮对话是不是由某条通道发起的（那样的轮次自有回复渠道）。
func IsChannel(name string) bool {
	if name == "" {
		return false
	}
	regMu.RLock()
	defer regMu.RUnlock()
	for _, d := range declared {
		if d.Name == name {
			return true
		}
	}
	return false
}

// SetRouter 安装路由，传 nil 清除。单所有者：后装的覆盖先装的。
//
// 一并清掉组内会话锚（见 group.go）：装卸路由意味着分通道的开关或配置变了，此前那个
// 锚属于旧配置。留着它，重新启用时会把各通道拉回一个可能早已不该用的会话。
func SetRouter(r Router) {
	regMu.Lock()
	router, anchor = r, ""
	regMu.Unlock()
}

// Target 返回该会话的回复应发往的通道名；未安装路由时为空串。
func Target(sessionID string) string {
	regMu.RLock()
	r := router
	regMu.RUnlock()
	if r == nil {
		return ""
	}
	// 在锁外调用：Router 属于另一个插件，它自己也要加锁，持锁跨进去就是死锁的配方
	return r(sessionID)
}

// ServedBy 报告某条通道是否负责该会话的投递。未安装路由、或路由不指定时都算负责
// ——这让没有配置分通道的场景保持原样。
func ServedBy(channel, sessionID string) bool {
	t := Target(sessionID)
	return t == "" || t == channel
}

// Deliver 把一段文本投到指定通道上该会话对应的用户，返回是否真的交给了平台。
// 原样一条发出；助手的最终回复走 deliverTo 的 paced 路径，由目标通道按自己的开关分条。
func Deliver(ctx context.Context, channel, sessionID, text string) bool {
	return deliverTo(ctx, channel, sessionID, text, false)
}

func deliverTo(ctx context.Context, channel, sessionID, text string, paced bool) bool {
	regMu.RLock()
	core := liveCores[channel]
	regMu.RUnlock()
	if core == nil {
		log.Printf("imbot: 通道 %s 当前未启用，无法投递", channel)
		return false
	}
	return core.pushTo(ctx, sessionID, text, paced)
}

func registerLive(c *Core) {
	if c.cfg.PluginName == "" {
		return
	}
	regMu.Lock()
	liveCores[c.cfg.PluginName] = c
	regMu.Unlock()
}

// unregisterLive 只在登记的就是自己时注销。Init 可重入，重配时会先 Stop 旧 Core
// 再 Start 新的；万一两者次序颠倒，无条件删除会把新的那份一并抹掉。
func unregisterLive(c *Core) {
	regMu.Lock()
	if liveCores[c.cfg.PluginName] == c {
		delete(liveCores, c.cfg.PluginName)
	}
	regMu.Unlock()
}

// pushTo 找出本通道上该会话对应的用户并推送。
//
// 收件人的选法决定了会话的连续性：分通道意味着两条通道要落在同一个会话上，
// 否则两个人格待在两个会话里，可见域分区无从谈起。所以本通道还没有人绑到这个
// 会话时，会把「唯一一个跟机器人说过话的用户」接过来。
//
// 已知用户不止一个就放弃，不广播——把一侧人格的话发给该通道上每一个聊过天的人，
// 是这个功能能造成的最坏后果，宁可不送达。
func (c *Core) pushTo(ctx context.Context, sessionID, text string, paced bool) bool {
	if c.cfg.Push == nil {
		log.Printf("%s: 未提供主动推送能力，无法接收转投", c.cfg.PluginName)
		return false
	}
	if !c.Running() {
		return false
	}

	users := c.binding.UsersFor(sessionID)
	if len(users) == 0 {
		known := c.binding.Users()
		if len(known) != 1 {
			log.Printf("%s: 没有可投递的用户（已知 %d 人，需要恰好 1 人才能自动接入会话）",
				c.cfg.PluginName, len(known))
			return false
		}
		prev := c.binding.Get(known[0])
		if err := c.binding.Set(known[0], sessionID); err != nil {
			log.Printf("%s: 保存会话映射失败: %v", c.cfg.PluginName, err)
			return false
		}
		if prev != "" && prev != sessionID {
			// 接管改写了一个已有的绑定：那个用户原本在别的会话里，此刻被搬了过来。
			// 装了会话锚之后这该是罕见事（各通道本就共用一个会话），留痕是为了万一
			// 再漂时有迹可循——此前它是完全无声的，表现出来就是「切回来接不上了」。
			log.Printf("%s: 用户 %s 由会话 %s 改绑到 %s（分通道投递）", c.cfg.PluginName, known[0], prev, sessionID)
			c.notice(ctx, sessionID, i18n.Tf(c.cfg.Lang, "imbot.notice.rebound",
				"「%s」上的用户原本在会话 %s 里，已改绑到当前会话。", c.cfg.PluginName, prev))
		} else {
			log.Printf("%s: 已把用户 %s 接到会话 %s（分通道投递）", c.cfg.PluginName, known[0], sessionID)
		}
		users = known
	}

	pctx := c.lifeCtx(ctx)
	ok := false
	for _, u := range users {
		var sent bool
		if paced {
			sent = c.pushPaced(pctx, u, text)
		} else {
			sent = c.cfg.Push(pctx, u, text)
		}
		if sent {
			ok = true
		}
	}
	return ok
}

// lifeCtx 优先用本 Core 自己的生命周期 ctx：转投的调用方是另一条通道的 worker，
// 它的 ctx 随那一轮结束就取消了，而这边的发送还在路上。
func (c *Core) lifeCtx(fallback context.Context) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx != nil {
		return c.ctx
	}
	return fallback
}

// sortedKeys 让「已知用户」的顺序稳定，日志与测试才可读。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
