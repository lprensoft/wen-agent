package growth

import (
	"errors"
	"slices"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// errDetailGone 表示状态页点的那条小变化已经不在库里了。
var errDetailGone = errors.New("growth detail not found")

// EditState 处理状态页上的成长修正。
//
// 人在这里改与模型在反思里改走同一条存储路径：改枝叶、放下都会留一行成长记录，
// 也都会进可撤销的历史；不同的是这里不受反思轮次与每日限额约束——用户是批准方，
// 修正一条写错的记录不需要再等一天。核心（已批准的大变化）不在这里改。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.growth.state.err.not_ready", "成长记录尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(p.domains(), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.growth.state.err.unknown_target", "没有要修改的这一块。"))
	}
	switch req.Op {
	case stateOpApprove, stateOpReject:
		// 与设置页的批准按钮同一条规则：凭据里的域与提议身份对不上就拒绝。
		_, err := p.decide(req.Lang, req.Op == stateOpApprove, req.Domain+"/"+req.Row)
		return err
	case stateOpEdit, stateOpLetGo:
		return p.editDetail(req)
	}
	return errors.New(i18n.T(req.Lang, "plugin.growth.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) editDetail(req plugin.StateEditRequest) error {
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.growth.state.err.not_ready", "成长记录尚未就绪。"))
	}
	now := time.Now()
	if p.now != nil {
		now = p.now()
	}

	var text, basis, why string
	if req.Op == stateOpEdit {
		var err error
		if text, err = editText(req.Lang, req.Values["text"], maxTextRunes); err != nil {
			return err
		}
		if basis, err = editText(req.Lang, req.Values["basis"], maxBasisRunes); err != nil {
			return err
		}
		if why, err = editText(req.Lang, req.Values["why"], maxWhyRunes); err != nil {
			return err
		}
	}

	_, err := store.Update(func(st *State) (bool, []LogEntry, error) {
		i := st.detailIndex(req.Row)
		if i < 0 {
			return false, nil, errDetailGone
		}
		before := st.Details[i]
		if req.Op == stateOpLetGo {
			st.Details = append(st.Details[:i], st.Details[i+1:]...)
			st.pushHistory(Change{At: now, Layer: layerDetail, Op: opLetGo, Before: &before})
			return true, []LogEntry{{At: now, Layer: layerDetail, Op: opLetGo, Text: before.Text}}, nil
		}
		after := before
		after.Text, after.Basis, after.Why = text, basis, why
		if after.Text == before.Text && after.Basis == before.Basis && after.Why == before.Why {
			return false, nil, nil
		}
		st.Details[i] = after
		st.pushHistory(Change{At: now, Layer: layerDetail, Op: opModify, Before: &before, After: &after})
		return true, []LogEntry{{
			At: now, Layer: layerDetail, Op: opModify,
			Text: after.Text, Prev: before.Text, Basis: after.Basis, Why: after.Why,
		}}, nil
	})
	if err != nil {
		if errors.Is(err, errDetailGone) {
			return errors.New(i18n.T(req.Lang, "plugin.growth.state.err.no_such", "这条小变化已经不在了。"))
		}
		i18n.Logf(p.langOf(), "log.growth.state_edit_failed", "状态页修改成长失败：%v", err)
		return errors.New(i18n.T(req.Lang, "plugin.growth.state.err.save_failed", "保存成长记录失败，请稍后重试。"))
	}
	return nil
}

// editText 校验一个人工输入的字段：去首尾空白、不能空、不能超长。
func editText(lang, v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New(i18n.T(lang, "plugin.growth.state.err.field_required", "这一项不能为空。"))
	}
	if n := len([]rune(v)); n > max {
		return "", errors.New(i18n.Tf(lang, "plugin.growth.state.err.too_long",
			"内容太长（%d 字，上限 %d 字）。", n, max))
	}
	return v, nil
}

var _ plugin.StateEditor = (*Plugin)(nil)
