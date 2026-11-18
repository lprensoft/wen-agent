package mood

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// 状态页上的操作标识。静态、不翻译。
const (
	stateSection = "mood"
	stateOpSet   = "set"
	stateOpReset = "reset"
)

// moodEdits 是这一域心情的修正入口：绝对设值，或重置。
func moodEdits(lang string, st State) []plugin.StateEdit {
	return []plugin.StateEdit{
		{
			Op:    stateOpSet,
			Label: i18n.T(lang, "plugin.mood.state.set", "调整"),
			Fields: []plugin.StateField{
				{
					Key: "value", Label: i18n.T(lang, "plugin.mood.state.field.value", "心情（-100 到 100）"),
					Type: plugin.StateFieldNumber, Value: strconv.Itoa(st.Value), Required: true,
					Min: plugin.IntPtr(moodMin), Max: plugin.IntPtr(moodMax),
				},
				{
					Key: "reason", Label: i18n.T(lang, "plugin.mood.state.field.reason", "起因"),
					Type: plugin.StateFieldText, Value: st.Reason, MaxLen: maxReasonRunes,
				},
			},
		},
		{
			Op:      stateOpReset,
			Label:   i18n.T(lang, "plugin.mood.state.reset", "重置"),
			Confirm: i18n.T(lang, "plugin.mood.state.reset_confirm", "清空这个域的心情记录？"),
			Danger:  true,
		},
	}
}

// EditState 处理状态页上的心情修正。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.mood.state.err.not_ready", "心情记录尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(s.base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.mood.state.err.unknown_target", "没有要修改的这一块。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.mood.state.err.not_ready", "心情记录尚未就绪。"))
	}

	switch req.Op {
	case stateOpSet:
		return p.setMood(req, store)
	case stateOpReset:
		if err := store.Clear(); err != nil {
			i18n.Logf(p.langOf(), "log.mood.state_edit_failed", "状态页修改心情失败：%v", err)
			return errors.New(i18n.T(req.Lang, "plugin.mood.state.err.save_failed", "保存心情失败，请稍后重试。"))
		}
		return nil
	}
	return errors.New(i18n.T(req.Lang, "plugin.mood.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) setMood(req plugin.StateEditRequest, store *Store) error {
	value, err := strconv.Atoi(strings.TrimSpace(req.Values["value"]))
	if err != nil || value < moodMin || value > moodMax {
		return errors.New(i18n.T(req.Lang, "plugin.mood.state.err.bad_value", "心情要是 -100 到 100 的整数。"))
	}
	reason := strings.TrimSpace(req.Values["reason"])
	if n := len([]rune(reason)); n > maxReasonRunes {
		return errors.New(i18n.Tf(req.Lang, "plugin.mood.state.err.too_long",
			"起因太长（%d 字，上限 %d 字）。", n, maxReasonRunes))
	}
	if _, err := store.Set(value, reason, time.Now()); err != nil {
		i18n.Logf(p.langOf(), "log.mood.state_edit_failed", "状态页修改心情失败：%v", err)
		return errors.New(i18n.T(req.Lang, "plugin.mood.state.err.save_failed", "保存心情失败，请稍后重试。"))
	}
	return nil
}

var _ plugin.StateEditor = (*Plugin)(nil)
