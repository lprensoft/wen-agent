package health

import (
	"errors"
	"slices"
	"strings"
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
	var view View
	found := false
	for _, v := range snap.Active {
		if v.Cond.ID == req.Row {
			cond, view, found = v.Cond, v, true
			break
		}
	}
	if !found {
		return errors.New(i18n.T(req.Lang, "plugin.health.state.err.no_such", "这条状况已经不在了。"))
	}

	switch req.Op {
	case stateOpEdit:
		// 新界面会带回打开表单时的值，用户没动过的项一律不写。
		sendSeverity := req.Touched("severity") && req.Originals != nil
		sendCare := req.Touched("care")
		if req.Originals == nil {
			// 旧客户端不带 Originals，只能按提交了哪些字段判断意图。
			rawSev := strings.TrimSpace(req.Values["severity"])
			rawCare := strings.TrimSpace(req.Values["care"])
			switch {
			case rawSev == "" && rawCare == "":
				sendSeverity, sendCare = false, false
			case rawSev == "":
				// 只提交处理方式：意图明确。严重度键可能带着空值一起提交，
				// 同样不能让它进入严重度校验。
				sendSeverity, sendCare = false, true
			case rawCare == "":
				// 只提交严重度：意图明确，直接执行；与当前档位一致则视为没改，
				// 免得一次原样提交把病程进度重新拨回加重段。care 键可能存在但
				// 值为空（旧客户端会把空值一起带上），不能让它进入处理方式校验。
				sendCare = false
				if sev, ok := severities.Canonical(rawSev); !ok || sev != viewSeverityWord(view) {
					sendSeverity = true
				} else {
					sendSeverity = false
				}
			default:
				// 整表提交：严重度与当前表单档位一致就是「没动过这一项」；
				// 不一致时无法区分用户主动调档还是病程在表单打开后自己跨了档，
				// 与其猜，不如让客户端刷新后再提交。
				if sev, ok := severities.Canonical(rawSev); ok && sev == viewSeverityWord(view) {
					sendSeverity = false
				} else {
					return errors.New(i18n.T(req.Lang, "plugin.health.state.err.stale",
						"这一页已经过期，请刷新后再操作。"))
				}
				sendCare = true
			}
		}

		u := Update{}
		if sendSeverity {
			sev, ok := severities.Canonical(req.Values["severity"])
			if !ok {
				return errors.New(i18n.T(req.Lang, "plugin.health.state.err.bad_severity", "严重度不在给定的几档里。"))
			}
			u.Severity = &sev
		}
		if sendCare {
			care, ok := careKinds.Canonical(req.Values["care"])
			if !ok {
				return errors.New(i18n.T(req.Lang, "plugin.health.state.err.bad_care", "处理方式不在给定的几档里。"))
			}
			if care != cond.Care {
				u.Care = &care
			}
		}
		if u.Severity == nil && u.Care == nil {
			return nil
		}
		if _, _, err := store.Apply(cond.Name, u, now, s.limits()); err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		return nil
	case stateOpRecover:
		if _, _, err := store.Apply(cond.Name, Update{Recovered: true}, now, s.limits()); err != nil {
			return p.stateEditFailed(req.Lang, err)
		}
		// 已经入队、心跳还没领取的「到点发作了」随记录一起作废。
		dropCue(req.Domain, cond.ID)
		return nil
	}
	return errors.New(i18n.T(req.Lang, "plugin.health.state.err.unknown_op", "不认识的修改操作。"))
}

func (p *Plugin) stateEditFailed(lang string, err error) error {
	i18n.Logf(p.langOf(), "log.health.state_edit_failed", "状态页修改身体状况失败：%v", err)
	return errors.New(i18n.T(lang, "plugin.health.state.err.save_failed", "保存身体状况失败，请稍后重试。"))
}

var _ plugin.StateEditor = (*Plugin)(nil)
