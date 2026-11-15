package mood

import (
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// State 把各可见域的心情交给设置页的「角色状态」，并给聊天页顶部贡献一行摘要。
// 心情属于人格：表里两面各有各的一份，状态页枚举全部域，由界面折叠其它人格。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	s := p.snapshot()
	if s.base == "" {
		return nil
	}
	now := time.Now()

	var out []plugin.StateSection
	for _, tag := range plugin.ReadDomains(s.base, plugin.Scope{}) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		st, ok, err := store.Current(s.decayPerHour, now)
		if err != nil {
			i18n.Logf(p.langOf(), "log.mood.state_failed", "读取心情失败（域 %q）: %v", tag, err)
			continue
		}
		if !ok {
			continue
		}
		band := moodBandLabel(q.Lang, st.Value)
		// 状态页标题已经是「心情」，摘要不再重复前缀；顶部条有「此刻：」领起。
		summary := band
		if st.Reason != "" {
			summary = st.Reason
		}
		row := plugin.StateRow{
			Label: i18n.T(q.Lang, "plugin.mood.state.value", "心情"),
			Value: band,
		}
		if st.Reason != "" {
			row.Detail = st.Reason
		}
		out = append(out, plugin.StateSection{
			Key:         "mood",
			Title:       i18n.T(q.Lang, "plugin.mood.state.title", "心情"),
			Summary:     summary,
			Rows:        []plugin.StateRow{row},
			Domain:      tag,
			Pinned:      true,
			PinnedOrder: 2,
			UpdatedAt:   st.Updated,
		})
	}
	return out
}

// moodBandLabel 是界面侧的心情分档。与注入给模型的那一份分档一致，但走 i18n：
// 界面文案与提示词语言包按受众分开维护。
func moodBandLabel(lang string, v int) string {
	switch {
	case v <= -70:
		return i18n.T(lang, "plugin.mood.state.band.despondent", "消沉")
	case v <= -40:
		return i18n.T(lang, "plugin.mood.state.band.sad", "难过")
	case v <= -15:
		return i18n.T(lang, "plugin.mood.state.band.down", "低落")
	case v < 15:
		return i18n.T(lang, "plugin.mood.state.band.calm", "平静")
	case v < 40:
		return i18n.T(lang, "plugin.mood.state.band.pleased", "愉快")
	case v < 70:
		return i18n.T(lang, "plugin.mood.state.band.happy", "开心")
	default:
		return i18n.T(lang, "plugin.mood.state.band.elated", "雀跃")
	}
}
