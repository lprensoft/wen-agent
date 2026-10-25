package growth

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// 设置页上的操作：批准或驳回那条在等的大变化、撤销最近一次改变、查看成长记录、清空。
//
// 批准只在这里做，不交给模型：让角色在对话里问一句、由模型判断对方答应了，等于让模型
// 自己批准自己。「同一时间只有一条在等」正好对上操作入口的形状——两个按钮，说明里接着
// 那一条的原文，不需要任何新的界面机制。
//
// 用户拥有全部可见域，这里不做可见域过滤：界面上的操作不是模型的读取路径。

const (
	actionApprove = "approve"
	actionReject  = "reject"
	actionUndo    = "undo"
	actionLog     = "log"
	actionClear   = "clear"
)

// logView 是「查看成长记录」列出的条数。
const logView = 30

// Actions 给批准与驳回两个按钮挂上展示凭据：说明里显示的是哪一条提议（域与它的身份），
// 按下时界面原样带回来，见 decide。
func (p *Plugin) Actions() []plugin.ActionDef {
	pending, token := "· —", ""
	if tag, st, ok := p.nextPending(); ok {
		pending = "· " + st.Pending.Text
		token = pendingKey(tag, *st.Pending)
	}
	return []plugin.ActionDef{
		{Key: actionApprove, Label: "批准这条改变", Description: "让角色提议的那个大变化生效：", Detail: pending, Token: token},
		{Key: actionReject, Label: "驳回这条改变", Description: "不接受角色提议的那个大变化，角色照旧：", Detail: pending, Token: token},
		{Key: actionUndo, Label: "撤销最近一次改变", Description: "把最近一次成长（新增、修改、放下或批准）还原回去。"},
		{Key: actionLog, Label: "查看成长记录", Description: "最近的改变，连同依据与「为什么这还是自己」。"},
		{Key: actionClear, Label: "清空全部成长", Description: "回到只有角色设定的状态，包括各人格分开保存的那几份。成长记录保留。不可撤销。"},
	}
}

// domains 列出磁盘上已有的全部可见域。
func (p *Plugin) domains() []string {
	s := p.snapshot()
	if s.base == "" {
		return nil
	}
	return plugin.ReadDomains(s.base, plugin.Scope{})
}

// nextPending 找全部域里**最早提出**的那条待批准提议。按提出时间而不是按域的顺序：
// 后来在另一个人格里新提的一条永远排在后面，顶不掉界面上已经显示的那一条。
func (p *Plugin) nextPending() (string, State, bool) {
	var (
		bestTag string
		best    State
		found   bool
	)
	for _, tag := range p.domains() {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		st, err := store.Load()
		if err != nil || st.Pending == nil {
			continue
		}
		if !found || st.Pending.Since.Before(best.Pending.Since) {
			bestTag, best, found = tag, st, true
		}
	}
	return bestTag, best, found
}

// pendingKey 是一条提议的身份：哪个域、它的随机身份。它就是批准与驳回按钮的展示凭据。
// 不用编号：清空成长会把编号一并重置，新提议会再拿到 c1，旧窗口上的凭据就又对上了。
func pendingKey(tag string, e Entry) string { return tag + "/" + entryIdent(e) }

// entryIdent 取提议的身份。随机身份是后来加的，早先存下的提议没有，退到编号。
func entryIdent(e Entry) string {
	if e.Nonce != "" {
		return e.Nonce
	}
	return "id:" + e.ID
}

// parsePendingKey 拆开展示凭据，并只认磁盘上真有的域：凭据来自客户端，域名会被拿去
// 拼目录，不能任它指向别处。
func (p *Plugin) parsePendingKey(token string) (tag, ident string, ok bool) {
	i := strings.LastIndex(token, "/")
	if i < 0 || i == len(token)-1 {
		return "", "", false
	}
	tag, ident = token[:i], token[i+1:]
	if !slices.Contains(p.domains(), tag) {
		return "", "", false
	}
	return tag, ident, true
}

func (p *Plugin) StartAction(ctx context.Context, key string) error {
	lang := p.langOf()
	var (
		msg string
		err error
	)
	switch key {
	case actionApprove, actionReject:
		msg, err = p.decide(lang, key == actionApprove, plugin.ActionTokenFrom(ctx))
	case actionUndo:
		msg, err = p.undo(lang)
	case actionLog:
		msg, err = p.renderLog(lang)
	case actionClear:
		msg, err = p.clearAll(lang)
	default:
		return plugin.UnknownAction(lang, key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if err != nil {
		p.actState[key] = plugin.ActionState{Status: plugin.ActionError, Message: err.Error()}
		return err
	}
	p.actState[key] = plugin.ActionState{Status: plugin.ActionDone, Message: msg}
	return nil
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	switch key {
	case actionApprove, actionReject, actionUndo, actionLog, actionClear:
	default:
		return plugin.ActionState{}, plugin.UnknownAction(p.langOf(), key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if st, ok := p.actState[key]; ok {
		return st, nil
	}
	return plugin.ActionState{Status: plugin.ActionIdle}, nil
}

// decide 批准或驳回按钮上显示的那一条提议——由请求带回的展示凭据指明，不由这里挑。
//
// 按下时重新挑一次「下一条」是不够的，记住「最后一次展示的是哪条」也不够（那是全局的，
// 别的窗口一刷新就改了）：窗口 A 停在提议 A 上，窗口 B 驳回了 A、刷新出 B，A 上的按钮
// 按下去就成了批准 B。所以只认凭据里的域与提议的随机身份，写入时在锁内核对那一条还在
// 等；不在了（别处已经处理掉、或整个清空了）或者没带凭据，一律拒绝并请用户刷新。
func (p *Plugin) decide(lang string, approve bool, token string) (string, error) {
	stale := fmt.Errorf("%s", i18n.T(lang, "plugin.growth.action.stale",
		"等待批准的改变已经变了，请刷新后看清楚再操作。"))
	if token == "" {
		if _, _, ok := p.nextPending(); !ok {
			return i18n.T(lang, "plugin.growth.action.no_pending", "现在没有在等批准的改变。"), nil
		}
		return "", stale
	}
	tag, ident, ok := p.parsePendingKey(token)
	if !ok {
		return "", stale
	}
	now := p.now()
	var text string
	_, err := p.storeFor(tag).Update(func(st *State) (bool, []LogEntry, error) {
		if st.Pending == nil || entryIdent(*st.Pending) != ident {
			return false, nil, stale
		}
		e := *st.Pending
		text = e.Text
		st.Pending = nil
		if !approve {
			return true, []LogEntry{{At: now, Layer: layerCore, Op: opReject, Text: e.Text}}, nil
		}
		e.Since = now
		st.Core = append(st.Core, e)
		st.pushHistory(Change{At: now, Layer: layerCore, Op: opApprove, After: &e})
		return true, []LogEntry{{At: now, Layer: layerCore, Op: opApprove, Text: e.Text, Basis: e.Basis, Why: e.Why}}, nil
	})
	if err != nil {
		return "", err
	}
	if text == "" {
		return i18n.T(lang, "plugin.growth.action.no_pending", "现在没有在等批准的改变。"), nil
	}
	if approve {
		return i18n.Tf(lang, "plugin.growth.action.approved", "已批准：%s", text), nil
	}
	return i18n.Tf(lang, "plugin.growth.action.rejected", "已驳回：%s", text), nil
}

// undo 还原全部域里最近的那一次改动。
func (p *Plugin) undo(lang string) (string, error) {
	var (
		latestTag string
		latestAt  time.Time
		found     bool
	)
	for _, tag := range p.domains() {
		st, err := p.storeFor(tag).Load()
		if err != nil || len(st.History) == 0 {
			continue
		}
		if at := st.History[len(st.History)-1].At; !found || at.After(latestAt) {
			latestTag, latestAt, found = tag, at, true
		}
	}
	if !found {
		return i18n.T(lang, "plugin.growth.action.nothing_to_undo", "没有可以撤销的改变。"), nil
	}
	now := p.now()
	var desc string
	_, err := p.storeFor(latestTag).Update(func(st *State) (bool, []LogEntry, error) {
		if len(st.History) == 0 {
			return false, nil, nil
		}
		c := st.History[len(st.History)-1]
		st.History = st.History[:len(st.History)-1]
		switch {
		case c.Layer == layerCore: // 撤销批准：从已定下来的里拿掉，不回到待批准
			st.Core = removeID(st.Core, c.After.ID)
			desc = c.After.Text
		case c.Op == opAdd:
			st.Details = removeID(st.Details, c.After.ID)
			desc = c.After.Text
		case c.Op == opModify:
			if i := st.detailIndex(c.After.ID); i >= 0 {
				st.Details[i] = *c.Before
			}
			desc = c.Before.Text
		case c.Op == opLetGo:
			st.Details = append(st.Details, *c.Before)
			desc = c.Before.Text
		}
		return true, []LogEntry{{At: now, Layer: c.Layer, Op: opUndo, Text: desc, Prev: c.Op}}, nil
	})
	if err != nil {
		return "", err
	}
	return i18n.Tf(lang, "plugin.growth.action.undone", "已撤销：%s", desc), nil
}

func removeID(list []Entry, id string) []Entry {
	out := list[:0]
	for _, e := range list {
		if e.ID != id {
			out = append(out, e)
		}
	}
	return out
}

// renderLog 汇总全部域的成长记录，按时间排，取最近 logView 条，最新在前。
func (p *Plugin) renderLog(lang string) (string, error) {
	var all []LogEntry
	for _, tag := range p.domains() {
		lines, err := p.storeFor(tag).ReadLog(logView)
		if err != nil {
			return "", err
		}
		all = append(all, lines...)
	}
	if len(all) == 0 {
		return i18n.T(lang, "plugin.growth.action.log_empty", "还没有任何成长记录。"), nil
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	if len(all) > logView {
		all = all[:logView]
	}
	var b strings.Builder
	for _, l := range all {
		at := l.At.Format("2006-01-02 15:04")
		if l.Text != "" {
			b.WriteString(i18n.Tf(lang, "plugin.growth.action.log_line", "%s  %s：%s", at, opLabel(lang, l), l.Text))
		} else {
			b.WriteString(at + "  " + opLabel(lang, l))
		}
		if l.Prev != "" && l.Op == opModify {
			b.WriteString(i18n.Tf(lang, "plugin.growth.action.log_prev", "（原来是「%s」）", l.Prev))
		}
		b.WriteString("\n")
		if l.Basis != "" {
			b.WriteString(i18n.Tf(lang, "plugin.growth.action.log_basis", "    依据：%s", l.Basis) + "\n")
		}
		if l.Why != "" {
			b.WriteString(i18n.Tf(lang, "plugin.growth.action.log_why", "    为什么还是自己：%s", l.Why) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// opLabel 是一行记录的动作说法。
func opLabel(lang string, l LogEntry) string {
	switch l.Op {
	case opAdd:
		return i18n.T(lang, "plugin.growth.op.add", "新的小变化")
	case opModify:
		return i18n.T(lang, "plugin.growth.op.modify", "小变化改了")
	case opLetGo:
		return i18n.T(lang, "plugin.growth.op.let_go", "放下小变化")
	case opPropose:
		return i18n.T(lang, "plugin.growth.op.propose", "提议大变化")
	case opApprove:
		return i18n.T(lang, "plugin.growth.op.approve", "批准大变化")
	case opReject:
		return i18n.T(lang, "plugin.growth.op.reject", "驳回大变化")
	case opUndo:
		return i18n.T(lang, "plugin.growth.op.undo", "撤销")
	case opClear:
		return i18n.T(lang, "plugin.growth.op.clear", "清空全部成长")
	}
	return l.Op
}

// clearAll 清空全部域的现状，成长记录保留。
func (p *Plugin) clearAll(lang string) (string, error) {
	now := p.now()
	n := 0
	var failed []string
	for _, tag := range p.domains() {
		if err := p.storeFor(tag).Clear(now); err != nil {
			failed = append(failed, err.Error())
			continue
		}
		n++
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("%s", i18n.Tf(lang, "plugin.growth.action.clear_failed", "清空成长失败：%s", strings.Join(failed, "; ")))
	}
	return i18n.Tf(lang, "plugin.growth.action.cleared", "已清空 %d 份成长。", n), nil
}

// StatusLines 报一行成长概况；什么都还没长出来时不报。
func (p *Plugin) StatusLines(lang string) []string {
	details, core, pending := 0, 0, 0
	for _, tag := range p.domains() {
		st, err := p.storeFor(tag).Load()
		if err != nil {
			continue
		}
		details += len(st.Details)
		core += len(st.Core)
		if st.Pending != nil {
			pending++
		}
	}
	if details+core+pending == 0 {
		return nil
	}
	line := i18n.Tf(lang, "plugin.growth.status", "🌱 成长：小变化 %d 条，大变化 %d 条", details, core)
	if pending > 0 {
		line += i18n.Tf(lang, "plugin.growth.status.pending", "，%d 条等你批准", pending)
	}
	return []string{line}
}
