package health

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// State 把各可见域的身体状况交给设置页的「角色状态」。
// 身体状况属于人格：表里两面各有各的处境，状态页枚举全部域，由界面折叠其它人格。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	if s.base == "" {
		return nil
	}
	now := q.Now()

	var out []plugin.StateSection
	for _, tag := range plugin.ReadDomains(s.base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		snap, err := store.Current(now)
		if err != nil {
			i18n.Logf(p.langOf(), "log.health.state_failed", "读取身体状况失败（域 %q）: %v", tag, err)
			continue
		}
		var rows []plugin.StateRow
		for _, v := range snap.Active {
			if v.Phase == phaseRecovered {
				continue
			}
			tone := ""
			switch v.Phase {
			case phasePending:
				tone = plugin.StateToneMuted
			case phaseRising:
				tone = plugin.StateToneWarn
			}
			rows = append(rows, plugin.StateRow{
				Value:  v.Cond.Name,
				Detail: healthDetail(q.Lang, v, now),
				Tone:   tone,
				ID:     v.Cond.ID,
				Edits:  healthEdits(q.Lang, v),
			})
		}
		if len(rows) == 0 {
			continue
		}
		out = append(out, plugin.StateSection{
			Key:     "health",
			Title:   i18n.T(q.Lang, "plugin.health.state.title", "健康状况"),
			Summary: i18n.Tf(q.Lang, "plugin.health.state.summary", "共 %d 个状况", len(rows)),
			Rows:    rows,
			Domain:  tag,
		})
	}
	return out
}

// healthDetail 是状态页里的补充说明。行本身已经给出状况名，这里只写病程与
// 处理方式，不再把名字重复一遍。
func healthDetail(lang string, v View, now time.Time) string {
	if v.Phase == phasePending {
		return i18n.Tf(lang, "plugin.health.state.pending", "预计%s发作",
			untilStatus(lang, v.Cond.Onset.Sub(now)))
	}
	return i18n.Tf(lang, "plugin.health.state.day", "第 %d 天（%s）",
		v.Day, careStatus(lang, v.Cond.Care))
}

// 状态页上的操作标识。静态、不翻译。
const (
	stateSection   = "health"
	stateOpEdit    = "edit"
	stateOpRecover = "recover"
)

// healthEdits 是一条状况上的操作：改严重度与处理方式，或直接标记痊愈。
func healthEdits(lang string, v View) []plugin.StateEdit {
	return []plugin.StateEdit{
		{
			Op:    stateOpEdit,
			Label: i18n.T(lang, "plugin.health.state.edit", "调整"),
			Fields: []plugin.StateField{
				{
					Key: "severity", Label: i18n.T(lang, "plugin.health.state.field.severity", "严重度"),
					Type: plugin.StateFieldSelect, Value: viewSeverityWord(v), Options: severityOptions(lang),
				},
				{
					Key: "care", Label: i18n.T(lang, "plugin.health.state.field.care", "处理方式"),
					Type: plugin.StateFieldSelect, Value: v.Cond.Care, Options: careOptions(lang),
				},
			},
		},
		{
			Op:      stateOpRecover,
			Label:   i18n.T(lang, "plugin.health.state.recover", "标记痊愈"),
			Confirm: i18n.T(lang, "plugin.health.state.recover_confirm", "标记为已痊愈？之后不再显示这条状况。"),
		},
	}
}

// severityWord 由峰值反推回模型与存储里用的那个词。
func severityWord(peak int) string {
	switch {
	case peak >= 70:
		return sevSevere.ZH()
	case peak >= 40:
		return sevModerate.ZH()
	}
	return sevMild.ZH()
}

// viewSeverityWord 是表单里该回显的档位：已发作的状况按**此刻**的严重度，
// 还没发作的按预计峰值（此刻还没有严重度可言）。
//
// 降档只把进度拨到新档、不改历史峰值，所以已发作的状况不能再从 Peak 反推，
// 否则用户刚调到「有点不舒服」，重新打开表单又看到「难受」。
func viewSeverityWord(v View) string {
	if v.Phase == phasePending {
		return severityWord(v.Cond.Peak)
	}
	// 与病程注入用的 band 同一套分档（35 / 65），别让表单与角色上下文对同一
	// 个数值说成两个档。
	return activeSeverityWord(v.Severity)
}

func activeSeverityWord(v int) string {
	switch {
	case v <= 35:
		return sevMild.ZH()
	case v <= 65:
		return sevModerate.ZH()
	}
	return sevSevere.ZH()
}

func severityOptions(lang string) []plugin.StateOption {
	values := severities.ZHValues()
	out := make([]plugin.StateOption, 0, len(values))
	for _, v := range values {
		out = append(out, plugin.StateOption{Value: v, Label: severityLabel(lang, v)})
	}
	return out
}

func severityLabel(lang, v string) string {
	switch v {
	case sevMild.ZH():
		return i18n.T(lang, "plugin.health.state.severity.mild", "有点不舒服")
	case sevModerate.ZH():
		return i18n.T(lang, "plugin.health.state.severity.moderate", "难受")
	case sevSevere.ZH():
		return i18n.T(lang, "plugin.health.state.severity.severe", "病倒了")
	}
	return v
}

func careOptions(lang string) []plugin.StateOption {
	values := careKinds.ZHValues()
	out := make([]plugin.StateOption, 0, len(values))
	for _, v := range values {
		out = append(out, plugin.StateOption{Value: v, Label: careLabel(lang, v)})
	}
	return out
}

func careLabel(lang, v string) string {
	switch v {
	case careTough:
		return i18n.T(lang, "plugin.health.state.care.tough", "硬扛")
	case careMeds:
		return i18n.T(lang, "plugin.health.state.care.meds", "吃药")
	case careDoctor:
		return i18n.T(lang, "plugin.health.state.care.doctor", "就医")
	}
	return v
}
