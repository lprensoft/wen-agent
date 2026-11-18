package bodysense

import (
	"strconv"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateMaxParts 是状态页上最多列几个部位。部位表本身有上限，这里只兜页面长度。
const stateMaxParts = 30

// State 把各可见域的即时身体状态与接触记录交给设置页的「角色状态」。
//
// 两块分开：身体是此刻的数值（会随时间回落），接触记录是长期累计。
// 按域分块，其余人格域由界面折叠。
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
	failed := false
	for _, tag := range plugin.ReadDomains(s.base, plugin.Scope{}) {
		sec, ok, bad := p.bodySection(q, s, tag, now)
		if ok {
			out = append(out, sec)
		}
		failed = failed || bad
		sec, ok, bad = p.contactSection(q, tag)
		if ok {
			out = append(out, sec)
		}
		failed = failed || bad
	}

	if len(out) == 0 {
		if failed {
			// 读失败与「确实没有记录」是两件事：前者不该显示成空状态。
			return nil
		}
		return []plugin.StateSection{{
			Key:   "body",
			Title: i18n.T(q.Lang, "plugin.body_sense.state.title", "身体"),
			Empty: i18n.T(q.Lang, "plugin.body_sense.state.empty", "还没有身体记录。"),
		}}
	}
	return out
}

// bodySection 渲染一个域的即时身体状态。两项都为 0 且没有起因时不出块：
// 完全平复的身体不值得占一行。第三个返回值表示读取失败。
func (p *Plugin) bodySection(q plugin.StateQuery, s settings, tag string, now time.Time) (plugin.StateSection, bool, bool) {
	store := p.stateStoreFor(tag)
	if store == nil {
		return plugin.StateSection{}, false, false
	}
	st, ok, err := store.Current(s.arousalDecay, s.fatigueDecay, now)
	if err != nil {
		i18n.Logf(s.lang, "log.body_sense.state_failed", "读取身体状态失败（域 %q）: %v", tag, err)
		return plugin.StateSection{}, false, true
	}
	if !ok || (st.Arousal == 0 && st.Fatigue == 0 && st.Reason == "") {
		return plugin.StateSection{}, false, false
	}
	rows := make([]plugin.StateRow, 0, 3)
	if st.Arousal > 0 {
		rows = append(rows, plugin.StateRow{
			Label: i18n.T(q.Lang, "plugin.body_sense.state.arousal", "唤起"),
			Value: strconv.Itoa(st.Arousal),
		})
	}
	if st.Fatigue > 0 {
		rows = append(rows, plugin.StateRow{
			Label: i18n.T(q.Lang, "plugin.body_sense.state.fatigue", "疲劳"),
			Value: strconv.Itoa(st.Fatigue),
		})
	}
	if st.Reason != "" {
		rows = append(rows, plugin.StateRow{
			Label: i18n.T(q.Lang, "plugin.body_sense.state.reason", "起因"),
			Value: st.Reason,
		})
	}
	return plugin.StateSection{
		Key:       "body",
		Title:     i18n.T(q.Lang, "plugin.body_sense.state.title", "身体"),
		Rows:      rows,
		Domain:    tag,
		UpdatedAt: st.Updated,
	}, true, false
}

// contactSection 渲染一个域的累计接触记录。没有记录的域不出块。
func (p *Plugin) contactSection(q plugin.StateQuery, tag string) (plugin.StateSection, bool, bool) {
	store := p.storeFor(tag)
	if store == nil {
		return plugin.StateSection{}, false, false
	}
	states, err := store.List()
	if err != nil {
		i18n.Logf(p.langOf(), "log.body_sense.state_contacts_failed", "读取接触记录失败（域 %q）: %v", tag, err)
		return plugin.StateSection{}, false, true
	}
	rows := make([]plugin.StateRow, 0, min(len(states), stateMaxParts))
	var latest time.Time
	recorded := 0
	for _, st := range states {
		if st.Count <= 0 {
			continue
		}
		recorded++
		if st.Last.After(latest) {
			latest = st.Last
		}
		if len(rows) >= stateMaxParts {
			continue
		}
		rows = append(rows, plugin.StateRow{
			Meta:   i18n.Tf(q.Lang, "plugin.body_sense.state.times", "%d 次", st.Count),
			Value:  st.Part,
			Detail: st.LastAct,
		})
	}
	if recorded == 0 {
		return plugin.StateSection{}, false, false
	}
	return plugin.StateSection{
		Key:       "contacts",
		Title:     i18n.T(q.Lang, "plugin.body_sense.state.contacts_title", "接触记录"),
		Summary:   i18n.Tf(q.Lang, "plugin.body_sense.state.contacts_summary", "%d 个部位", recorded),
		Rows:      rows,
		Domain:    tag,
		UpdatedAt: latest,
		More:      recorded - len(rows),
	}, true, false
}
