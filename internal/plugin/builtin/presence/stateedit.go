package presence

import (
	"errors"
	"slices"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的现场修正。
//
// 六个字段一起提交：空串清除该字段，全部清空时 Store 自己把文件删掉。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.presence.state.err.not_ready", "现场状态尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.presence.state.err.unknown_target", "没有要修改的这一块。"))
	}
	if req.Op != stateOpEdit {
		return errors.New(i18n.T(req.Lang, "plugin.presence.state.err.unknown_op", "不认识的修改操作。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.presence.state.err.not_ready", "现场状态尚未就绪。"))
	}

	changes := make(map[string]string, len(fieldDefs))
	for _, d := range fieldDefs {
		raw, ok := req.Values[d.key]
		if !ok {
			continue
		}
		v := strings.TrimSpace(raw)
		if n := len([]rune(v)); n > d.maxRunes {
			return errors.New(i18n.Tf(req.Lang, "plugin.presence.state.err.too_long",
				"内容太长（%d 字，上限 %d 字）。", n, d.maxRunes))
		}
		changes[d.key] = v
	}
	if len(changes) == 0 {
		return nil
	}
	if _, _, err := store.Apply(changes, time.Now()); err != nil {
		i18n.Logf(p.snapshotLang(), "log.presence.state_edit_failed", "状态页修改现场失败：%v", err)
		return errors.New(i18n.T(req.Lang, "plugin.presence.state.err.save_failed", "保存现场失败，请稍后重试。"))
	}
	return nil
}

var _ plugin.StateEditor = (*Plugin)(nil)
