package bodysense

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的身体状态修正。
//
// 这里改的是某一个可见域的那一份记录：身体按域各存一份、读取时取最大值，
// 主人在页面上看到的是当前域，修正也落在当前域。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.body_sense.state.err.not_ready", "身体状态尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(s.base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.body_sense.state.err.unknown_target", "没有要修改的这一块。"))
	}
	store := p.stateStoreFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.body_sense.state.err.not_ready", "身体状态尚未就绪。"))
	}

	switch req.Op {
	case stateOpSet:
		return p.setBody(req, store, s)
	case stateOpReset:
		if err := store.Clear(); err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		return nil
	}
	return errors.New(i18n.T(req.Lang, "plugin.body_sense.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) setBody(req plugin.StateEditRequest, store *StateStore, s settings) error {
	var arousalPtr, fatiguePtr *int
	var reasonPtr *string
	if req.Touched("arousal") {
		v, err := bodyValue(req.Lang, req.Values["arousal"])
		if err != nil {
			return err
		}
		arousalPtr = &v
	}
	if req.Touched("fatigue") {
		v, err := bodyValue(req.Lang, req.Values["fatigue"])
		if err != nil {
			return err
		}
		fatiguePtr = &v
	}
	if req.Touched("reason") {
		v := strings.TrimSpace(req.Values["reason"])
		if n := len([]rune(v)); n > maxStateReasonRunes {
			return errors.New(i18n.Tf(req.Lang, "plugin.body_sense.state.err.too_long",
				"起因太长（%d 字，上限 %d 字）。", n, maxStateReasonRunes))
		}
		reasonPtr = &v
	}
	if arousalPtr == nil && fatiguePtr == nil && reasonPtr == nil {
		return nil
	}
	if _, err := store.SetFields(arousalPtr, fatiguePtr, reasonPtr, s.arousalDecay, s.fatigueDecay, p.now()); err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	return nil
}

// bodyValue 解析一个 0-100 的整数。空值不当成 0：修正表单总是带着现值，
// 空出来多半是清掉了输入框，静默设成 0 会把状态改错。
func bodyValue(lang, raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	n, err := strconv.Atoi(raw)
	if err != nil || n < stateMin || n > stateMax {
		return 0, errors.New(i18n.T(lang, "plugin.body_sense.state.err.bad_value", "唤起与疲劳要是 0-100 的整数。"))
	}
	return n, nil
}

func (p *Plugin) stateEditFailed(lang string, err error) error {
	i18n.Logf(p.langOf(), "log.body_sense.state_edit_failed", "状态页修改身体状态失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.body_sense.state.err.save_failed", "保存身体状态失败，请稍后重试。"))
}

var _ plugin.StateEditor = (*Plugin)(nil)
