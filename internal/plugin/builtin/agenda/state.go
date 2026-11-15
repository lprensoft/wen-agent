package agenda

import (
	"sort"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

const stateMaxRows = 10

// State 把今天的表与未来的约定交给设置页的「角色状态」。
//
// 表按可见域分开，一块对应一条故事线的一天；状态页是主人看的，所以枚举全部域，
// 每块挂 Domain 标签，由界面折叠其它人格。约定同理，单独一块。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	s := p.snapshot()
	if s.base == "" {
		return nil
	}
	now := p.now()
	day := s.today(now)
	today := day.Format(dateLayout)

	var out []plugin.StateSection
	planned := false
	failed := false
	for _, tag := range p.allDomains() {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		if pl, err := store.LoadPlan(); err != nil {
			i18n.Logf(p.langOf(), "log.agenda.state_failed", "读取日程状态失败（域 %q）: %v", tag, err)
			failed = true
		} else if pl.Date == today {
			if sec, ok := p.daySection(q.Lang, pl, day, s.dayStartHour, tag); ok {
				out = append(out, sec)
				planned = true
			}
		}
		if cs, err := store.LoadCommitments(); err != nil {
			i18n.Logf(p.langOf(), "log.agenda.state_failed", "读取日程状态失败（域 %q）: %v", tag, err)
			failed = true
		} else if sec, ok := p.commitSection(q.Lang, cs, today, tag); ok {
			out = append(out, sec)
		}
	}
	if !planned && len(out) == 0 {
		if failed {
			return nil
		}
		return []plugin.StateSection{{
			Key:   "day",
			Title: i18n.T(q.Lang, "plugin.agenda.state.day.title", "今日安排"),
			Empty: i18n.T(q.Lang, "plugin.agenda.state.day.empty", "今天还没有排表。"),
		}}
	}
	return out
}

// daySection 渲染一张表。取消的项不出现——它们不再占角色的一天；
// 已完成、跳过、延期的项保留，用灰色说明它们曾经在那里。
func (p *Plugin) daySection(lang string, pl Plan, day time.Time, dayStartHour int, domain string) (plugin.StateSection, bool) {
	items := make([]Item, 0, len(pl.Items))
	for _, it := range pl.Items {
		if it.Status == statusCancelled {
			continue
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return plugin.StateSection{}, false
	}
	sort.SliceStable(items, func(i, j int) bool {
		// 不能直接比 HH:MM：day_start_hour 之前的 01:00 属于次日凌晨，
		// 它排在 09:00 与 23:00 之后，而不是最前。
		si, _ := items[i].span(day, dayStartHour)
		sj, _ := items[j].span(day, dayStartHour)
		if si.IsZero() || sj.IsZero() {
			if items[i].Start != items[j].Start {
				return items[i].Start < items[j].Start
			}
			return items[i].ID < items[j].ID
		}
		if !si.Equal(sj) {
			return si.Before(sj)
		}
		return items[i].ID < items[j].ID
	})

	total, ongoing, done := 0, 0, 0
	rows := make([]plugin.StateRow, 0, len(items))
	for _, it := range items {
		total++
		switch it.Status {
		case statusOngoing:
			ongoing++
		case statusDone:
			done++
		}
		if len(rows) >= stateMaxRows {
			continue
		}
		detail := agendaStatusLabel(lang, it.Status)
		if it.Place != "" {
			detail += " · " + it.Place
		}
		tone := ""
		switch it.Status {
		case statusDone, statusSkipped, statusDeferred:
			tone = plugin.StateToneMuted
		}
		rows = append(rows, plugin.StateRow{
			Meta:   it.Start,
			Value:  it.Title,
			Detail: detail,
			Tone:   tone,
		})
	}

	summary := i18n.Tf(lang, "plugin.agenda.state.day.summary", "共 %d 项", total)
	if ongoing > 0 {
		summary = i18n.Tf(lang, "plugin.agenda.state.day.summary_ongoing",
			"共 %d 项 · 进行中 %d 项", total, ongoing)
	} else if done > 0 {
		summary = i18n.Tf(lang, "plugin.agenda.state.day.summary_done",
			"共 %d 项 · 已完成 %d 项", total, done)
	}
	return plugin.StateSection{
		Key:     "day",
		Title:   i18n.T(lang, "plugin.agenda.state.day.title", "今日安排"),
		Summary: summary,
		Rows:    rows,
		Domain:  domain,
		More:    total - len(rows),
	}, true
}

// commitSection 渲染未来的约定：只留今天及以后，按日期与时刻排序。
// 没有约定时不出现，不占一块「空的」版面。
func (p *Plugin) commitSection(lang string, all []Commitment, today, domain string) (plugin.StateSection, bool) {
	cs := make([]Commitment, 0, len(all))
	for _, c := range all {
		if c.Date >= today {
			cs = append(cs, c)
		}
	}
	if len(cs) == 0 {
		return plugin.StateSection{}, false
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Date != cs[j].Date {
			return cs[i].Date < cs[j].Date
		}
		return cs[i].Start < cs[j].Start
	})

	rows := make([]plugin.StateRow, 0, len(cs))
	for _, c := range cs {
		if len(rows) >= stateMaxRows {
			continue
		}
		detail := ""
		if c.Place != "" {
			detail = c.Place
		}
		if len(c.With) > 0 {
			if detail != "" {
				detail += " · "
			}
			sep := ", "
			if lang == "zh" {
				sep = "、"
			}
			detail += strings.Join(c.With, sep)
		}
		rows = append(rows, plugin.StateRow{
			Meta:   strings.TrimSpace(c.Date + " " + c.Start),
			Value:  c.Title,
			Detail: detail,
		})
	}
	return plugin.StateSection{
		Key:     "commitments",
		Title:   i18n.T(lang, "plugin.agenda.state.commit.title", "未来的约定"),
		Summary: i18n.Tf(lang, "plugin.agenda.state.commit.summary", "%d 条", len(cs)),
		Rows:    rows,
		Domain:  domain,
		More:    len(cs) - len(rows),
	}, true
}

// agendaStatusLabel 的键写成字面量，覆盖检查靠静态扫 (键, 中文) 这一对。
func agendaStatusLabel(lang, status string) string {
	switch status {
	case statusPlanned:
		return i18n.T(lang, "plugin.agenda.state.status.planned", "待开始")
	case statusOngoing:
		return i18n.T(lang, "plugin.agenda.state.status.ongoing", "进行中")
	case statusDone:
		return i18n.T(lang, "plugin.agenda.state.status.done", "已完成")
	case statusSkipped:
		return i18n.T(lang, "plugin.agenda.state.status.skipped", "跳过")
	case statusDeferred:
		return i18n.T(lang, "plugin.agenda.state.status.deferred", "延期")
	case statusCancelled:
		return i18n.T(lang, "plugin.agenda.state.status.cancelled", "已取消")
	}
	return status
}
