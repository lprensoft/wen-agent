package relationship

import (
	"errors"
	"slices"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的关系修正。阶段改了仍按原来的规则自动记下那一天，
// 与模型侧 update_relationship 走同一个 Store。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	p.mu.RLock()
	base := p.base
	p.mu.RUnlock()
	if base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.relationship.state.err.not_ready", "关系状态尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.relationship.state.err.unknown_target", "没有要修改的这一块。"))
	}
	if req.Op != stateOpEdit {
		return errors.New(i18n.T(req.Lang, "plugin.relationship.state.err.unknown_op", "不认识的修改操作。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.relationship.state.err.not_ready", "关系状态尚未就绪。"))
	}

	u := Update{}
	if raw, ok := req.Values["stage"]; ok {
		v, err := relText(req.Lang, raw, maxStageRunes)
		if err != nil {
			return err
		}
		u.Stage = &v
	}
	if raw, ok := req.Values["my_call"]; ok {
		v, err := relText(req.Lang, raw, maxCallRunes)
		if err != nil {
			return err
		}
		u.MyCall = &v
	}
	if raw, ok := req.Values["their_call"]; ok {
		v, err := relText(req.Lang, raw, maxCallRunes)
		if err != nil {
			return err
		}
		u.TheirCall = &v
	}
	if raw, ok := req.Values["recent"]; ok {
		v, err := relText(req.Lang, raw, maxRecentRunes)
		if err != nil {
			return err
		}
		u.Recent = &v
	}
	if raw, ok := req.Values["their_now"]; ok {
		v, err := relText(req.Lang, raw, maxTheirNowRunes)
		if err != nil {
			return err
		}
		u.TheirNow = &v
	}
	if raw, ok := req.Values["bonds"]; ok {
		bonds, err := relBonds(req.Lang, raw)
		if err != nil {
			return err
		}
		u.Bonds = &bonds
	}

	if _, _, err := store.Apply(u, time.Now()); err != nil {
		i18n.Logf(p.langOf(), "log.relationship.state_edit_failed", "状态页修改关系失败：%v", err)
		return errors.New(i18n.T(req.Lang, "plugin.relationship.state.err.save_failed", "保存关系失败，请稍后重试。"))
	}
	return nil
}

func relText(lang, raw string, max int) (string, error) {
	v := strings.TrimSpace(raw)
	if n := len([]rune(v)); n > max {
		return "", errors.New(i18n.Tf(lang, "plugin.relationship.state.err.too_long",
			"内容太长（%d 字，上限 %d 字）。", n, max))
	}
	return v, nil
}

func relBonds(lang, raw string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len([]rune(line)) > maxBondRunes {
			return nil, errors.New(i18n.Tf(lang, "plugin.relationship.state.err.too_long",
				"内容太长（%d 字，上限 %d 字）。", len([]rune(line)), maxBondRunes))
		}
		out = append(out, line)
	}
	if len(out) > maxBonds {
		return nil, errors.New(i18n.Tf(lang, "plugin.relationship.state.err.too_many",
			"默契与禁区最多 %d 条。", maxBonds))
	}
	return out, nil
}

var _ plugin.StateEditor = (*Plugin)(nil)
