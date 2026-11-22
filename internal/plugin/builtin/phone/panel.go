package phone

import (
	"sort"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stateMaxRows 是状态页最多列出的条数。手机里留了 30 条供 read_news 细看，
// 状态页只让人扫一眼最近刷到了什么。
const stateMaxRows = 10

// State 把最近刷到的消息交给设置页的「角色状态」。
//
// 手机不分人格（同一部手机刷到的是同一批），整块标为 Global。只读内存里的状态，
// 不联网、不写盘——与 StatusLines 同契约，随时可调。
func (p *Plugin) State(q plugin.StateQuery) []plugin.StateSection {
	if q.PinnedOnly {
		return nil
	}
	title := i18n.T(q.Lang, "plugin.phone.state.title", "手机")
	if len(p.snapshot().feeds) == 0 {
		return []plugin.StateSection{{
			Key:    "phone",
			Title:  title,
			Empty:  i18n.T(q.Lang, "plugin.phone.state.no_feeds", "还没有配置订阅源。"),
			Global: true,
		}}
	}

	p.dataMu.RLock()
	items := append([]Item(nil), p.st.Items...)
	last := p.st.LastRefresh
	errs := make(map[string]string, len(p.st.Errors))
	for k, v := range p.st.Errors {
		errs[k] = v
	}
	p.dataMu.RUnlock()

	sort.SliceStable(items, func(i, j int) bool { return items[i].when().After(items[j].when()) })
	total := len(items)
	more := 0
	if len(items) > stateMaxRows {
		more = len(items) - stateMaxRows
		items = items[:stateMaxRows]
	}

	rows := make([]plugin.StateRow, 0, len(items)+len(errs))
	for _, it := range items {
		// 标题与源名也来自外部，可能很长或带换行：同样折成一行并截断，
		// 免得一个坏源把整块顶穿字节上限。
		title := clipText(it.Title, 200)
		if title == "" {
			title = clipText(it.Source, 80)
		}
		row := plugin.StateRow{Value: title, Meta: clipText(it.Source, 80)}
		if s := clipText(it.Summary, 160); s != "" {
			row.Detail = s
		}
		rows = append(rows, row)
	}

	// 取不到的源单独列出来：否则状态页看起来只是「最近没刷到什么」，
	// 而实际原因是某个源坏了。
	urls := make([]string, 0, len(errs))
	for u := range errs {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	for _, u := range urls {
		rows = append(rows, plugin.StateRow{
			Label:  i18n.T(q.Lang, "plugin.phone.state.failed", "未能取到"),
			Value:  clipText(hostOf(u), 80),
			Detail: clipText(errs[u], 200),
			Tone:   plugin.StateToneWarn,
		})
	}

	// 超过 8 行的块在前端默认收起；摘要给条数与取不到的源数，收起时也知道
	// 这里有东西可看，而不是只剩一个「手机」。
	summary := ""
	if total > 0 {
		summary = i18n.Tf(q.Lang, "plugin.phone.state.summary", "共 %d 条", total)
	}
	if len(errs) > 0 {
		failed := i18n.Tf(q.Lang, "plugin.phone.state.summary_failed", "%d 个源取不到", len(errs))
		if summary == "" {
			summary = failed
		} else {
			summary += " · " + failed
		}
	}

	sec := plugin.StateSection{
		Key:       "phone",
		Title:     title,
		Summary:   summary,
		Rows:      rows,
		More:      more,
		Global:    true,
		UpdatedAt: last,
	}
	if len(rows) == 0 {
		sec.Empty = i18n.T(q.Lang, "plugin.phone.state.empty", "还没刷到什么。")
	}
	return []plugin.StateSection{sec}
}

// clipText 把订阅源给的摘要压成一行：换行与连续空白折成空格，超长截断。
// 摘要是外部内容，可能很长，也可能带换行；状态页一行放不下那么多。
func clipText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

var _ plugin.StateReporter = (*Plugin)(nil)
