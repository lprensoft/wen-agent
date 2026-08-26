package stylewatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wen/internal/prompt"
)

// reportTool 给出最近几天的文风趋势。它存在是为了让「最近是不是越来越像客服了」
// 这种问题能在对话里直接得到数字，而不必去翻设置页。
type reportTool struct{ p *Plugin }

func (t *reportTool) Name() string { return "style_report" }

func (t *reportTool) Description() string {
	return prompt.T(t.p.lang(), "prompt.style_watch.tool.style_report.desc",
		"查看最近 7 天回复文风的统计：每天的对话轮数、助手腔命中次数与种类、平均字数、【】演绎占比。"+
			"只在对方问起回复的文风、长度或是否变得像客服时使用。")
}

func (t *reportTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *reportTool) Execute(context.Context, json.RawMessage) (string, error) {
	return t.p.renderReport(t.p.lang(), 7), nil
}

// renderReport 渲染最近 n 天的报告，每天一行，新的在前；末尾汇总按规则的命中。
func (p *Plugin) renderReport(lang string, n int) string {
	days := p.recent(n)
	if len(days) == 0 {
		return prompt.Tf(lang, "prompt.style_watch.report.empty", "最近 %d 天没有记录。", n)
	}
	today := dateOf(time.Now())
	var b strings.Builder
	fmt.Fprintf(&b, prompt.T(lang, "prompt.style_watch.report.header", "最近 %d 天的文风（真人对话）：\n"), n)
	total := map[string]int{}
	turns, hits := 0, 0
	for _, d := range days {
		label := d.Date
		if len(label) == len("2006-01-02") {
			label = label[5:]
		}
		if d.Date == today {
			label += prompt.T(lang, "prompt.style_watch.report.today", "（今天）")
		}
		if d.Interactive.Turns == 0 {
			fmt.Fprintf(&b, prompt.T(lang, "prompt.style_watch.report.no_human", "%s：无真人对话"), label)
		} else {
			fmt.Fprintf(&b, prompt.T(lang, "prompt.style_watch.report.day", "%s：%s"), label, d.Interactive.summary(lang))
		}
		if bg := d.Background; bg.Turns > 0 {
			fmt.Fprintf(&b, prompt.T(lang, "prompt.style_watch.report.bg", "；后台 %d 轮 %d 次"), bg.Turns, bg.totalHits())
		}
		b.WriteString("\n")
		for id, c := range d.Interactive.Hits {
			total[id] += c
		}
		turns += d.Interactive.Turns
		hits += d.Interactive.totalHits()
	}
	fmt.Fprintf(&b, prompt.T(lang, "prompt.style_watch.report.total", "合计 %d 轮，助手腔 %d 次"), turns, hits)
	if bd := breakdown(lang, total, 0); bd != "" {
		fmt.Fprintf(&b, prompt.T(lang, "prompt.style_watch.report.paren", "（%s）"), bd)
	}
	b.WriteString(prompt.T(lang, "prompt.style_watch.report.period", "。"))
	return b.String()
}
