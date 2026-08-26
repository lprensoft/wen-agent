package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wen/internal/i18n"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 时间线：按日一行的「何时 + 什么转折」编年史。
//
// 长期记忆是按主题的（偏好/约定/事实/踩坑/经历），回答不了「我们最近经历了什么」——
// 隔了几天再开口，模型只能靠检索碰运气。时间线补上这条轴：每天的对话攒进当日
// 缓冲（不新增模型调用），新一天的第一轮对话触发一次收束，把昨天提炼成一行
// 「日期 | 关键事件」，最近若干条随 TurnPrompt 注入。
//
// 缓冲与时间线都按可见域分文件存在各域的记忆目录里：对话内容属于哪条故事线，
// 它的时间线也属于那条故事线。收束对每个域各跑一次，注入只合并本轮可读的域。

const (
	dayBufFileName   = "daybuf.json"
	timelineFileName = "timeline.json"

	// maxDayBufBytes 限制单日缓冲：收束要的是「这一天发生了什么」，超出时丢最早的
	// 部分——一天的要点极少藏在开头被冲掉的寒暄里。
	maxDayBufBytes = 16 * 1024
	// maxTimelineEntries 限制时间线长度：注入只取最近几条，更早的靠会话检索。
	maxTimelineEntries = 90
	// maxSummaryRunes 限制单条时间线：它逐轮进上下文，一行以内才是「时间线」。
	maxSummaryRunes = 120

	defaultTimeline     = true
	defaultTimelineDays = 7
)

// timelinePromptZH 把一天的对话收束成一行。SKIP 那条不能省：没有实质内容的一天
// 硬凑一行，时间线很快会被「聊了聊天」这类废话填满。
const timelinePromptZH = `下面是 %s 这一天的对话摘录。请把这一天概括成时间线上的一行记录：
这一天发生的关键事件、转折、决定或进展，一句话，不超过 60 字。
只输出这一句话本身，不带日期、不带引号、不带任何解释。
这一天没有实质内容（只是寒暄或没有要点的闲聊）时，只输出 SKIP。

对话摘录：
%s`

// timelineHeaderZH 领起本轮注入的时间线。
const timelineHeaderZH = `[最近几天]
按日的时间线，新的在前；提到某天的事而这里没有时，说明那天没有留下记录：`

// dayBuf 是一个自然日攒下的对话文本。
type dayBuf struct {
	Date string `json:"date"` // 2006-01-02，本地时区
	Text string `json:"text"`
}

// dayBufFile 允许同时存多天：收束靠模型调用，慢且可能失败，追加不能等它。
// 只有早于今天的日子会被收束取走，今天的那份继续攒。
type dayBufFile struct {
	Days []dayBuf `json:"days,omitempty"`
}

// timelineEntry 是时间线上的一行。
type timelineEntry struct {
	Date    string `json:"date"`
	Summary string `json:"summary"`
	// Facts 是收束当时各插件给出的当日客观事实（天气这类，见 plugin.DayReporter），
	// 渲染在正文之前，让这一行读起来像日记的抬头。
	//
	// 收束时取一次就存进来，不在注入时现问：日记是历史，那天的天气不该因为以后
	// 关掉天气插件、或它的日历滚过去了就消失。老条目没有这个字段，照旧渲染。
	Facts []string `json:"facts,omitempty"`
}

type timelineFile struct {
	Entries []timelineEntry `json:"entries,omitempty"`
}

func dayOf(t time.Time) string { return t.Local().Format("2006-01-02") }

// ---------- 当日缓冲 ----------

// appendDayBuf 把一轮对话追加进写入域的当日缓冲。在后台 goroutine 中执行，
// 文件访问由 timelineMu 串行化。
func (p *Plugin) appendDayBuf(s settings, tag string, ev plugin.TurnEndEvent) {
	dir := plugin.DomainDir(s.libBase, tag)
	var b strings.Builder
	if t := strings.TrimSpace(ev.UserInput); t != "" {
		b.WriteString(prompt.Tf(s.lang, "prompt.memory.dialogue.user", "用户: %s\n", t))
	}
	if t := strings.TrimSpace(ev.FinalText); t != "" {
		b.WriteString(prompt.Tf(s.lang, "prompt.memory.dialogue.assistant", "助手: %s\n", t))
	}
	if b.Len() == 0 {
		return
	}

	p.timelineMu.Lock()
	defer p.timelineMu.Unlock()
	f := loadDayBuf(dir)
	date := dayOf(ev.EndedAt)
	idx := -1
	for i := range f.Days {
		if f.Days[i].Date == date {
			idx = i
			break
		}
	}
	if idx < 0 {
		f.Days = append(f.Days, dayBuf{Date: date})
		idx = len(f.Days) - 1
	}
	text := f.Days[idx].Text + b.String()
	// 超限丢最早的部分，按行对齐，避免掐出半个多字节字符
	for len(text) > maxDayBufBytes {
		nl := strings.IndexByte(text, '\n')
		if nl < 0 {
			text = text[len(text)-maxDayBufBytes:]
			break
		}
		text = text[nl+1:]
	}
	f.Days[idx].Text = text
	saveDayBuf(s.lang, dir, f)
}

func loadDayBuf(dir string) dayBufFile {
	var f dayBufFile
	raw, err := os.ReadFile(filepath.Join(dir, dayBufFileName))
	if err != nil {
		return f
	}
	if json.Unmarshal(raw, &f) != nil {
		return dayBufFile{}
	}
	return f
}

// saveDayBuf 落盘缓冲。0600：内容是对话原文。失败只影响时间线的完整性，不打断对话。
func saveDayBuf(lang, dir string, f dayBufFile) {
	if len(f.Days) == 0 {
		_ = os.Remove(filepath.Join(dir, dayBufFileName))
		return
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, dayBufFileName), raw, 0o600); err != nil {
		i18n.Logf(lang, "log.memory.daybuf_write_failed", "时间线：当日缓冲写入失败：%v", err)
	}
}

// ---------- 日切收束 ----------

// runTimeline 把各域缓冲里早于今天的日子逐个收束成时间线记录。在后台 goroutine
// 中执行，一天触发一次（由 OnTurnEnd 的 lastTimeline 判定）。
func (p *Plugin) runTimeline(s settings) {
	defer func() {
		p.turnMu.Lock()
		p.timelining = false
		p.turnMu.Unlock()
	}()
	complete := p.completeFunc()
	if complete == nil || s.ctx == nil || s.libBase == "" {
		return
	}
	today := dayOf(time.Now())

	// 零值 Scope：枚举磁盘上已存在的全部域，每个域各自收束（与淡忘清扫同一口径）
	for _, tag := range plugin.ReadDomains(s.libBase, plugin.Scope{}) {
		dir := plugin.DomainDir(s.libBase, tag)

		p.timelineMu.Lock()
		f := loadDayBuf(dir)
		p.timelineMu.Unlock()

		for _, day := range f.Days {
			if day.Date >= today {
				continue // 今天的还在攒，动不得
			}
			if strings.TrimSpace(day.Text) == "" {
				p.dropDay(dir, day.Date)
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, extractTimeout)
			raw, err := complete(ctx, fmt.Sprintf(prompt.T(s.lang, "prompt.memory.timeline_prompt", timelinePromptZH), day.Date, day.Text))
			cancel()
			if err != nil {
				if s.ctx.Err() == nil {
					i18n.Logf(s.lang, "log.memory.timeline_close_failed", "时间线：收束 %s 失败，下次日切再试：%v", day.Date, err)
				}
				continue // 缓冲留着，明天的收束会连它一起处理
			}
			summary := clipSummary(raw)
			if summary != "" && !strings.EqualFold(summary, "SKIP") {
				e := timelineEntry{Date: day.Date, Summary: summary, Facts: p.factsFor(day.Date)}
				p.appendTimeline(dir, e)
				i18n.Logf(s.lang, "log.memory.timeline_entry", "时间线：%s | %s", day.Date, renderEntry(s.lang, e))
			}
			p.dropDay(dir, day.Date)
		}
	}
}

// dropDay 把某一天从缓冲里移除（已收束或不需要收束）。
func (p *Plugin) dropDay(dir, date string) {
	p.timelineMu.Lock()
	defer p.timelineMu.Unlock()
	f := loadDayBuf(dir)
	out := f.Days[:0]
	for _, d := range f.Days {
		if d.Date != date {
			out = append(out, d)
		}
	}
	f.Days = out
	saveDayBuf(p.langOf(), dir, f)
}

// appendTimeline 追加一行时间线并按日期排好，超出上限丢最早的。
func (p *Plugin) appendTimeline(dir string, e timelineEntry) {
	p.timelineMu.Lock()
	defer p.timelineMu.Unlock()
	f := loadTimeline(dir)
	f.Entries = append(f.Entries, e)
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Date < f.Entries[j].Date })
	if len(f.Entries) > maxTimelineEntries {
		f.Entries = f.Entries[len(f.Entries)-maxTimelineEntries:]
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, timelineFileName), raw, 0o600); err != nil {
		i18n.Logf(p.langOf(), "log.memory.timeline_write_failed", "时间线：写入失败：%v", err)
	}
}

func loadTimeline(dir string) timelineFile {
	var f timelineFile
	raw, err := os.ReadFile(filepath.Join(dir, timelineFileName))
	if err != nil {
		return f
	}
	if json.Unmarshal(raw, &f) != nil {
		return timelineFile{}
	}
	return f
}

// clipSummary 规整收束结果：压掉换行，剥包裹引号，限死长度。
func clipSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, "\"“”「」『』")
	if r := []rune(s); len(r) > maxSummaryRunes {
		s = string(r[:maxSummaryRunes])
	}
	return strings.TrimSpace(s)
}

// ---------- 注入 ----------

// timelineBlock 渲染本轮可读域合并后的时间线，最近的在前，最多 s.timelineDays 条。
// 没有记录时返回空串。条目长度有硬上限，整块体量有界，不需要预算与降级。
func (p *Plugin) timelineBlock(ctx context.Context, s settings) string {
	if !s.timeline || s.libBase == "" {
		return ""
	}
	var all []timelineEntry
	p.timelineMu.Lock()
	for _, tag := range plugin.ReadDomains(s.libBase, plugin.ScopeFrom(ctx)) {
		all = append(all, loadTimeline(plugin.DomainDir(s.libBase, tag)).Entries...)
	}
	p.timelineMu.Unlock()
	if len(all) == 0 {
		return ""
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Date > all[j].Date })
	if s.timelineDays > 0 && len(all) > s.timelineDays {
		all = all[:s.timelineDays]
	}
	var b strings.Builder
	b.WriteString(prompt.T(s.lang, "prompt.memory.timeline_header", timelineHeaderZH))
	for _, e := range all {
		b.WriteString("\n- ")
		b.WriteString(renderEntry(s.lang, e))
	}
	return b.String()
}

// renderEntry 把一行渲染成日记的样子：日期、星期几、当日事实、正文，逗号分隔。
//
//	2026-08-21 周五，天气：小雨 24~31℃，陪你聊了加班的事，答应炖排骨汤等你回来
//
// 星期几由日期算出来，不占存储：日记里「周五」比「2026-08-21」更能唤起那天是什么样。
// 认不出的日期（不该出现，除非文件被外部改过）就只写日期本身。
func renderEntry(lang string, e timelineEntry) string {
	var b strings.Builder
	b.WriteString(e.Date)
	if t, err := time.ParseInLocation("2006-01-02", e.Date, time.Local); err == nil {
		b.WriteString(" ")
		b.WriteString(weekdayName(lang, t))
	}
	sep := prompt.T(lang, "prompt.memory.timeline.sep", "，")
	for _, f := range e.Facts {
		b.WriteString(sep)
		b.WriteString(f)
	}
	b.WriteString(sep)
	b.WriteString(e.Summary)
	return b.String()
}

// weekdaysZH 是七个星期名，一行一个，从周日排起。整表一个键：七个键在别的语言里
// 只会被译得七零八落，而它们本来就该一起换。
const weekdaysZH = "周日\n周一\n周二\n周三\n周四\n周五\n周六"

func weekdayName(lang string, t time.Time) string {
	names := strings.Split(prompt.T(lang, "prompt.memory.weekdays", weekdaysZH), "\n")
	if len(names) != 7 {
		names = strings.Split(weekdaysZH, "\n") // 译文行数不对就退回中文，别越界
	}
	return names[int(t.Weekday())]
}

// factsFor 问一遍各插件那一天有什么可说的。入口不可用（评测装配、插件单测）时
// 返回 nil，日记照写，只是没有抬头那几句。
func (p *Plugin) factsFor(date string) []string {
	fn := p.dayFactsFunc()
	if fn == nil {
		return nil
	}
	return fn(date)
}
