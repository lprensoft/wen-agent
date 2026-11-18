package health

import (
	"errors"
	"slices"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// EditState 处理状态页上的身体状况修正：改严重度/处理方式，或标记痊愈。
//
// 与模型侧的 update_health 走同一个 Store；严重度改动仍按原来的病程折算规则
// 重算峰值，痊愈了就从下一次读取起消失。
func (p *Plugin) EditState(req plugin.StateEditRequest) error {
	s := p.snapshot()
	if s.base == "" {
		return errors.New(i18n.T(req.Lang, "plugin.health.state.err.not_ready", "身体状况尚未就绪。"))
	}
	if req.Section != stateSection || !slices.Contains(plugin.ReadDomains(s.base, plugin.Scope{}), req.Domain) {
		return errors.New(i18n.T(req.Lang, "plugin.health.state.err.unknown_target", "没有要修改的这一块。"))
	}
	store := p.storeFor(req.Domain)
	if store == nil {
		return errors.New(i18n.T(req.Lang, "plugin.health.state.err.not_ready", "身体状况尚未就绪。"))
	}
	now := time.Now()
	snap, err := store.Current(now)
	if err != nil {
		return p.stateEditFailed(req.Lang, err)
	}
	var cond Condition
	found := false
	for _, v := range snap.Active {
		if v.Cond.ID == req.Row {
			cond, found = v.Cond, true
			break
		}
	}
	if !found {
		return errors.New(i18n.T(req.Lang, "plugin.health.state.err.no_such", "这条状况已经不在了。"))
	}

	switch req.Op {
	case stateOpEdit:
		sev, ok := severities.Canonical(req.Values["severity"])
		if !ok {
			return errors.New(i18n.T(req.Lang, "plugin.health.state.err.bad_severity", "严重度不在给定的几档里。"))
		}
		care, ok := careKinds.Canonical(req.Values["care"])
		if !ok {
			return errors.New(i18n.T(req.Lang, "plugin.health.state.err.bad_care", "处理方式不在给定的几档里。"))
		}
		if _, _, err := store.Apply(cond.Name, Update{Severity: &sev, Care: &care}, now, s.limits()); err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		return nil
	case stateOpRecover:
		if _, _, err := store.Apply(cond.Name, Update{Recovered: true}, now, s.limits()); err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		return nil
	}
	return errors.New(i18n.T(req.Lang, "plugin.health.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) stateEditFailed(lang string, err error) error {
	i18n.Logf(p.langOf(), "log.health.state_edit_failed", "状态页修改身体状况失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.health.state.err.save_failed", "保存身体状况失败，请稍后重试。"))
}

var _ plugin.StateEditor = (*Plugin)(nil)
