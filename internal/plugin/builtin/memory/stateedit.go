package memory

import (
	"errors"
	"slices"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的记忆修正。
//
// 域名、行身份与操作都来自客户端：先按磁盘上真实存在的域核对，再把改动交给 Store
// 或时间线的写入口——校验、原子写、缓存失效与向量索引的哈希重算都在那条路上。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.libBase == "" {
		return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.not_ready", "记忆库尚未就绪。"))
	}
	if !slices.Contains(plugin.ReadDomains(s.libBase, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.unknown_target", "没有要修改的这一块。"))
	}
	switch req.Section {
	case stateSectionIndex:
		return p.editIndex(req)
	case stateSectionDiary:
		return p.editDiary(req, s.libBase)
	}
	return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.unknown_target", "没有要修改的这一块。"))
}

func (p *Plugin) editIndex(req plugin.StateEditRequest) error {
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.not_ready", "记忆库尚未就绪。"))
	}
	switch req.Op {
	case stateOpEdit:
		cur, err := store.GetBySlug(req.Row)
		if err != nil {
			return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.no_such", "这条记忆已经不在了。"))
		}
		desc := cur.Description
		if req.Touched("description") {
			desc = collapseSpace(req.Values["description"])
			if n := len([]rune(desc)); n > descMaxRunes {
				return errors.New(i18n.Tf(req.Lang, "plugin.memory.state.err.too_long",
					"内容太长（%d 字，上限 %d 字）。", n, descMaxRunes))
			}
		}
		typ := cur.Type
		if req.Touched("type") {
			typ = strings.TrimSpace(req.Values["type"])
			if typ != "" {
				if _, ok := memTypes.Canonical(typ); !ok {
					return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.bad_type", "分类不在给定的几档里。"))
				}
			}
		}
		if _, err := store.UpdateIndexBySlug(req.Row, desc, typ, time.Now()); err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		return nil
	case stateOpDelete:
		if _, err := store.GetBySlug(req.Row); err != nil {
			return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.no_such", "这条记忆已经不在了。"))
		}
		if _, err := store.DeleteBySlug(req.Row); err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		return nil
	}
	return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) editDiary(req plugin.StateEditRequest, base string) error {
	if _, err := time.ParseInLocation(time.DateOnly, req.Row, time.Local); err != nil {
		return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.no_such_day", "这一天的日记已经不在了。"))
	}
	dir := plugin.DomainDir(base, req.Domain)
	switch req.Op {
	case stateOpEdit:
		summary := strings.TrimSpace(collapseSpace(req.Values["summary"]))
		if summary == "" {
			return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.summary_required", "摘要不能为空。"))
		}
		if n := len([]rune(summary)); n > maxSummaryRunes {
			return errors.New(i18n.Tf(req.Lang, "plugin.memory.state.err.too_long",
				"内容太长（%d 字，上限 %d 字）。", n, maxSummaryRunes))
		}
		err := p.updateTimeline(dir, req.Row, summary)
		if errors.Is(err, errNoSuchDay) {
			return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.no_such_day", "这一天的日记已经不在了。"))
		}
		if err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		return nil
	case stateOpDelete:
		err := p.deleteTimeline(dir, req.Row)
		if errors.Is(err, errNoSuchDay) {
			return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.no_such_day", "这一天的日记已经不在了。"))
		}
		if err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		return nil
	}
	return errors.New(i18n.T(req.Lang, "plugin.memory.state.err.unknown_op", "不认识的修改操作。"))
}

// stateEditFailed 把库里的错误留给日志，界面只看到一句通用说明：库的报错是写给
// 模型的（按进程语言），混进请求语言的界面里会一半中文一半英文。
func (p *Plugin) stateEditFailed(lang string, err error) error {
	i18n.Logf(p.langOf(), "log.memory.state_edit_failed", "状态页修改记忆失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.memory.state.err.save_failed", "保存记忆失败，请稍后重试。"))
}

var _ plugin.StateEditor = (*Plugin)(nil)
