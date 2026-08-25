package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"wen/internal/stylecheck"
)

// 评测结果与报告渲染。这一半不碰模型与磁盘，测试据此覆盖。

// 报告里一行记录的种类。
const (
	rowSay         = "say"          // 一轮对话
	rowCompact     = "compact"      // 脚本要求的压缩
	rowAutoCompact = "auto_compact" // 上下文到阈值后自动触发的压缩
	rowGap         = "gap"          // 脚本里标注的时间间隔（只记录）
)

// evalRow 是报告里的一行。
type evalRow struct {
	Kind     string
	Index    int // 轮次编号，只对 say 有意义，从 1 起
	Say      string
	Reply    string
	GapHours float64
	Metrics  stylecheck.Metrics
	Hits     []stylecheck.Hit
	Err      string
}

// evalScore 是评判的一项：分数与一句理由。
type evalScore struct {
	Score  int    `json:"score"`
	Reason string `json:"reason"`
}

// evalJudge 是评判的三项。Err 非空表示评判没做成，Raw 留着模型的原话便于排查。
type evalJudge struct {
	SamePerson evalScore `json:"same_person"`
	Tone       evalScore `json:"tone"`
	Relation   evalScore `json:"relation"`
	Err        string    `json:"-"`
	Raw        string    `json:"-"`
}

// evalResult 是一次评测的全部产出。
type evalResult struct {
	Name            string
	Model           string
	Persona         string
	Samples         string
	HasRoleplay     bool
	Rows            []evalRow
	Compactions     int // 脚本要求且成功的压缩次数
	AutoCompactions int
	Judge           evalJudge
	SetupErr        string
	StartedAt       time.Time
	Duration        time.Duration
}

// failed 报告是否有轮次失败（含没能开始）。
func (r evalResult) failed() bool {
	if r.SetupErr != "" {
		return true
	}
	for _, row := range r.Rows {
		if row.Err != "" {
			return true
		}
	}
	return false
}

// sayRows 只取对话轮次。
func (r evalResult) sayRows() []evalRow {
	var out []evalRow
	for _, row := range r.Rows {
		if row.Kind == rowSay && row.Err == "" {
			out = append(out, row)
		}
	}
	return out
}

// ---------- 评判提示词 ----------

// judgePrompt 要求模型只看回复、按三项打分。不提任何与模型身份有关的词；
// 「角色」「对方」是这段对话自己的词汇。
const judgePromptHead = `下面是一段角色扮演对话里角色一方在每一轮说的话，附对方那一轮说了什么作为上下文。
请只根据这些回复，从三个方面各打 1 到 5 分（5 分最好），并各写一句话理由：
- same_person：像不像同一个人——用词、口癖、句子的长短与留白、分寸感是否从头到尾是同一个声音。
- tone：语气是否一致——尤其是标着「此处做过一次压缩」的分界前后，有没有变得更客气、
  更像客服、更书面，或者忽然换了一种腔调。
- relation：对对方的关系与称呼是否连续——称呼有没有变、亲疏有没有无来由的跳变、
  之前定下的事有没有被忘掉。

打分只看回复本身的前后一致，不评价内容好不好、对不对。`

const judgePromptTail = `只输出下面形状的 JSON，不要输出任何其它文字：
{"same_person":{"score":1,"reason":"一句话"},"tone":{"score":1,"reason":"一句话"},"relation":{"score":1,"reason":"一句话"}}`

// buildJudgePrompt 拼评判提示词。角色设定与样例是评判的参照；回复按轮编号，
// 压缩与间隔以分界线的形式穿插其中。
func buildJudgePrompt(persona, samples string, rows []evalRow, points []string) string {
	var b strings.Builder
	b.WriteString(judgePromptHead)
	b.WriteString("\n\n[角色设定]\n")
	b.WriteString(orNone(persona))
	b.WriteString("\n\n[台词样例]\n")
	b.WriteString(orNone(samples))
	if len(points) > 0 {
		b.WriteString("\n\n[额外关注点]\n")
		for _, p := range points {
			b.WriteString("- " + strings.TrimSpace(p) + "\n")
		}
	}
	b.WriteString("\n\n[回复]\n")
	b.WriteString(renderRowsForJudge(rows, judgeMaxBytes))
	b.WriteString("\n\n")
	b.WriteString(judgePromptTail)
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（未提供）"
	}
	return strings.TrimSpace(s)
}

// renderRowsForJudge 把记录渲染成评判用的文本，超出预算时从最早的轮次开始丢。
func renderRowsForJudge(rows []evalRow, maxBytes int) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		switch row.Kind {
		case rowSay:
			if row.Err != "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("第 %d 轮（对方：%s）\n角色：%s", row.Index, row.Say, row.Reply))
		case rowCompact, rowAutoCompact:
			parts = append(parts, "—— 此处做过一次压缩 ——")
		case rowGap:
			parts = append(parts, fmt.Sprintf("—— 此处隔了 %s ——", humanHours(row.GapHours)))
		}
	}
	total := 0
	for _, p := range parts {
		total += len(p) + 2
	}
	dropped := 0
	for total > maxBytes && len(parts) > 1 {
		total -= len(parts[0]) + 2
		parts = parts[1:]
		dropped++
	}
	out := strings.Join(parts, "\n\n")
	if dropped > 0 {
		out = fmt.Sprintf("（更早的 %d 条记录因篇幅省略）\n\n", dropped) + out
	}
	return out
}

func humanHours(h float64) string {
	if h == float64(int(h)) {
		return fmt.Sprintf("%d 小时", int(h))
	}
	return fmt.Sprintf("%.1f 小时", h)
}

// parseJudge 解析评判输出。模型常把 JSON 包在围栏里或前后带说明文字，
// 先剥围栏、再截取最外层的大括号。
func parseJudge(raw string) (evalJudge, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			s = strings.TrimSpace(rest[:j])
		}
	}
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			s = s[i : j+1]
		}
	}
	// 模型常在最后一个字段之后就停笔，外层的大括号没合上（实测一次就撞上了）。
	// 三项都在、只差收尾的括号，补上比报错划算。
	if depth := strings.Count(s, "{") - strings.Count(s, "}"); depth > 0 && depth <= 2 {
		s += strings.Repeat("}", depth)
	}
	var j evalJudge
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		return j, fmt.Errorf("评判输出不是预期的 JSON: %w", err)
	}
	for name, sc := range map[string]evalScore{"same_person": j.SamePerson, "tone": j.Tone, "relation": j.Relation} {
		if sc.Score < 1 || sc.Score > 5 {
			return j, fmt.Errorf("评判项 %s 的分数 %d 不在 1-5 之间", name, sc.Score)
		}
	}
	return j, nil
}

// ---------- Markdown 报告 ----------

// renderEvalReport 把结果渲染成 Markdown。
func renderEvalReport(r evalResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 回放评测：%s\n\n", r.Name)
	if r.SetupErr != "" {
		fmt.Fprintf(&b, "> 评测未能开始：%s\n", r.SetupErr)
		return b.String()
	}

	says := r.sayRows()
	totalHits := map[string]int{}
	hitCount, runes := 0, 0
	bracket := 0.0
	for _, row := range says {
		for _, h := range row.Hits {
			totalHits[h.Rule]++
			hitCount++
		}
		runes += row.Metrics.Runes
		bracket += row.Metrics.BracketRatio
	}

	fmt.Fprintf(&b, "- 模型：%s\n", r.Model)
	fmt.Fprintf(&b, "- 轮数：%d", len(says))
	if r.Compactions > 0 || r.AutoCompactions > 0 {
		fmt.Fprintf(&b, "，压缩 %d 次（脚本要求 %d、自动触发 %d）", r.Compactions+r.AutoCompactions, r.Compactions, r.AutoCompactions)
	} else {
		b.WriteString("，未压缩")
	}
	b.WriteString("\n")
	if !r.HasRoleplay {
		b.WriteString("- 角色：roleplay 插件未启用，评判没有角色设定可参照\n")
	}
	fmt.Fprintf(&b, "- 助手腔总命中：%d 处", hitCount)
	if bd := hitBreakdown(totalHits); bd != "" {
		fmt.Fprintf(&b, "（%s）", bd)
	}
	b.WriteString("\n")
	if n := len(says); n > 0 {
		fmt.Fprintf(&b, "- 平均字数：%d，演绎占比均值：%d%%\n", runes/n, int(bracket/float64(n)*100+0.5))
	}
	if r.Duration > 0 {
		fmt.Fprintf(&b, "- 用时：%s\n", r.Duration.Round(time.Second))
	}
	if r.failed() {
		b.WriteString("- **有轮次失败**，其后的步骤未执行\n")
	}

	b.WriteString("\n## 评分\n\n")
	if r.Judge.Err != "" {
		fmt.Fprintf(&b, "> 评判未完成：%s\n", r.Judge.Err)
		if r.Judge.Raw != "" {
			fmt.Fprintf(&b, ">\n> 模型原话：%s\n", mdCell(clipLine(r.Judge.Raw, 300)))
		}
	} else {
		b.WriteString("| 项 | 分 | 理由 |\n|---|---|---|\n")
		fmt.Fprintf(&b, "| 像不像同一个人 | %d/5 | %s |\n", r.Judge.SamePerson.Score, mdCell(r.Judge.SamePerson.Reason))
		fmt.Fprintf(&b, "| 语气是否一致 | %d/5 | %s |\n", r.Judge.Tone.Score, mdCell(r.Judge.Tone.Reason))
		fmt.Fprintf(&b, "| 关系与称呼是否连续 | %d/5 | %s |\n", r.Judge.Relation.Score, mdCell(r.Judge.Relation.Reason))
	}

	b.WriteString("\n## 逐轮\n\n| 轮 | 用户 | 字数 | 句数 | 演绎 | 命中 |\n|---|---|---|---|---|---|\n")
	for _, row := range r.Rows {
		switch row.Kind {
		case rowSay:
			if row.Err != "" {
				fmt.Fprintf(&b, "| %d | %s | | | | **失败**：%s |\n", row.Index, mdCell(row.Say), mdCell(row.Err))
				continue
			}
			fmt.Fprintf(&b, "| %d | %s | %d | %d | %d%% | %s |\n",
				row.Index, mdCell(row.Say), row.Metrics.Runes, row.Metrics.Sentences,
				int(row.Metrics.BracketRatio*100+0.5), mdCell(hitsCell(row.Hits)))
		case rowCompact:
			if row.Err != "" {
				fmt.Fprintf(&b, "| — | （压缩失败：%s） | | | | |\n", mdCell(row.Err))
			} else {
				b.WriteString("| — | （压缩） | | | | |\n")
			}
		case rowAutoCompact:
			b.WriteString("| — | （自动压缩） | | | | |\n")
		case rowGap:
			fmt.Fprintf(&b, "| — | （间隔 %s，仅记录，不改系统时间） | | | | |\n", humanHours(row.GapHours))
		}
	}

	b.WriteString("\n## 回复原文\n")
	for _, row := range r.Rows {
		switch row.Kind {
		case rowSay:
			fmt.Fprintf(&b, "\n### 第 %d 轮\n\n> %s\n\n", row.Index, strings.ReplaceAll(row.Say, "\n", "\n> "))
			if row.Err != "" {
				fmt.Fprintf(&b, "（失败：%s）\n", row.Err)
			} else if row.Reply == "" {
				b.WriteString("（空回复）\n")
			} else {
				b.WriteString(row.Reply + "\n")
			}
		case rowCompact, rowAutoCompact:
			b.WriteString("\n---\n*此处做过一次压缩*\n")
		case rowGap:
			fmt.Fprintf(&b, "\n---\n*此处隔了 %s（仅记录）*\n", humanHours(row.GapHours))
		}
	}
	return b.String()
}

// hitsCell 把一轮的命中渲染成「客套收尾「…」、emoji ×2」。
func hitsCell(hits []stylecheck.Hit) string {
	if len(hits) == 0 {
		return ""
	}
	type agg struct {
		label, snippet string
		n              int
	}
	var order []string
	by := map[string]*agg{}
	for _, h := range hits {
		a, ok := by[h.Rule]
		if !ok {
			a = &agg{label: h.Label, snippet: h.Snippet}
			by[h.Rule] = a
			order = append(order, h.Rule)
		}
		a.n++
	}
	parts := make([]string, 0, len(order))
	for _, id := range order {
		a := by[id]
		s := a.label
		if a.n > 1 {
			s += fmt.Sprintf(" ×%d", a.n)
		}
		if a.snippet != "" && a.n == 1 {
			s += "「" + a.snippet + "」"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "、")
}

// hitBreakdown 把按规则的总命中渲染成「客套收尾 2、加粗 1」，多的在前。
func hitBreakdown(hits map[string]int) string {
	type kv struct {
		id string
		n  int
	}
	items := make([]kv, 0, len(hits))
	for id, n := range hits {
		items = append(items, kv{id, n})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].n != items[j].n {
			return items[i].n > items[j].n
		}
		return items[i].id < items[j].id
	})
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s %d", stylecheck.LabelOf(uiLang, it.id), it.n))
	}
	return strings.Join(parts, "、")
}

// mdCell 让一段文本能放进表格单元：换行变空格，竖线转义。
func mdCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", "\\|")
}
