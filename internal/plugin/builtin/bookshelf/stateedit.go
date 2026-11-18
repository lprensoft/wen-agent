package bookshelf

import (
	"errors"
	"slices"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的读书修正：把一本书的进度与印象清零。
//
// 书架上的书本身是用户放进去的文件，不在这里删；清掉的只是「读到哪儿、想过什么」。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.bookshelf.state.err.not_ready", "读书进度尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.bookshelf.state.err.unknown_target", "没有要修改的这一块。"))
	}
	if req.Op != stateOpReset {
		return errors.New(i18n.T(req.Lang, "plugin.bookshelf.state.err.unknown_op", "不认识的修改操作。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.bookshelf.state.err.not_ready", "读书进度尚未就绪。"))
	}

	st, err := store.Load()
	if err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	if _, ok := st.Books[req.Row]; !ok {
		return errors.New(i18n.T(req.Lang, "plugin.bookshelf.state.err.no_such", "这本书的进度已经不在了。"))
	}
	if err := store.Update(func(st *State) error {
		delete(st.Books, req.Row)
		if st.Current == req.Row {
			st.Current = ""
		}
		return nil
	}); err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	return nil
}

func (p *Plugin) stateEditFailed(lang string, err error) error {
	i18n.Logf(p.langOf(), "log.bookshelf.state_edit_failed", "状态页修改读书进度失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.bookshelf.state.err.save_failed", "保存读书进度失败，请稍后重试。"))
}

var _ plugin.StateEditor = (*Plugin)(nil)
