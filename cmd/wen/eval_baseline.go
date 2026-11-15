package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"wen/internal/stylecheck"
)

// 结构化结果与基线对比。
//
// 报告是给人看的，基线要比的是机器：同一份脚本前后两次运行，逐轮比较确定性指标。
// schema 变了就升版本，旧基线直接拒绝，免得拿两套字段硬比。

const evalJSONSchema = 1

// evalJSON 是 --json 写出的形状，同时也是 --baseline 读的形状。
type evalJSON struct {
	Schema          int            `json:"schema"`
	Name            string         `json:"name"`
	Model           string         `json:"model"`
	Compactions     int            `json:"compactions"`
	AutoCompactions int            `json:"auto_compactions"`
	Turns           []evalJSONTurn `json:"turns"`
	Judge           *evalJSONJudge `json:"judge,omitempty"`
}

// evalJSONTurn 是一轮 say 的结构化结果。只留前后可比的数字与稳定 id，
// 不加会随模型措辞轻微变化的东西。
type evalJSONTurn struct {
	Index        int            `json:"index"`
	Say          string         `json:"say"`
	Reply        string         `json:"reply"`
	Runes        int            `json:"runes"`
	Sentences    int            `json:"sentences"`
	BracketRatio float64        `json:"bracket_ratio"`
	HitCounts    map[string]int `json:"hit_counts,omitempty"`
	Tools        []string       `json:"tools,omitempty"`
	ExpectFails  []string       `json:"expect_fails,omitempty"`
	Err          string         `json:"error,omitempty"`
}

// evalJSONJudge 是那次模型评判的三项分数。它只作趋势，对比时不据此判失败。
type evalJSONJudge struct {
	SamePerson int `json:"same_person"`
	Tone       int `json:"tone"`
	Relation   int `json:"relation"`
}

// evalJudgeDelta 是两次评分的对照，只用来在报告里写一行趋势。
type evalJudgeDelta struct {
	BaseSame, CurSame         int
	BaseTone, CurTone         int
	BaseRelation, CurRelation int
}

// evalJSONOf 把运行结果转成可落盘、可对比的形状。
func evalJSONOf(r evalResult) evalJSON {
	out := evalJSON{
		Schema:          evalJSONSchema,
		Name:            r.Name,
		Model:           r.Model,
		Compactions:     r.Compactions,
		AutoCompactions: r.AutoCompactions,
	}
	for _, row := range r.Rows {
		if row.Kind != rowSay {
			continue
		}
		t := evalJSONTurn{
			Index:        row.Index,
			Say:          row.Say,
			Reply:        row.Reply,
			Runes:        row.Metrics.Runes,
			Sentences:    row.Metrics.Sentences,
			BracketRatio: row.Metrics.BracketRatio,
			Tools:        row.Tools,
			ExpectFails:  row.ExpectFails,
			Err:          row.Err,
		}
		if len(row.Hits) > 0 {
			t.HitCounts = map[string]int{}
			for _, h := range row.Hits {
				t.HitCounts[h.Rule]++
			}
		}
		out.Turns = append(out.Turns, t)
	}
	if r.Judge.Err == "" && r.Judge.SamePerson.Score > 0 {
		out.Judge = &evalJSONJudge{
			SamePerson: r.Judge.SamePerson.Score,
			Tone:       r.Judge.Tone.Score,
			Relation:   r.Judge.Relation.Score,
		}
	}
	return out
}

// writeEvalJSON 写出结构化结果，供下一次 --baseline 对比。
func writeEvalJSON(path string, r evalResult) error {
	raw, err := json.MarshalIndent(evalJSONOf(r), "", "  ")
	if err != nil {
		return fmt.Errorf(tr("cli.eval.err.json", "写结构化结果失败: %w"), err)
	}
	if err := os.WriteFile(path, append(raw, byte(10)), 0o644); err != nil {
		return fmt.Errorf(tr("cli.eval.err.json", "写结构化结果失败: %w"), err)
	}
	return nil
}

// loadEvalJSON 读取上一次的结构化结果。schema 不认识就拒绝，不去猜字段。
func loadEvalJSON(path string) (evalJSON, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return evalJSON{}, fmt.Errorf(tr("cli.eval.err.baseline_read", "读取基线失败: %w"), err)
	}
	var b evalJSON
	if err := json.Unmarshal(raw, &b); err != nil {
		return evalJSON{}, fmt.Errorf(tr("cli.eval.err.baseline_read", "读取基线失败: %w"), err)
	}
	if b.Schema != evalJSONSchema {
		return evalJSON{}, fmt.Errorf(tr("cli.eval.err.baseline_schema", "基线文件的 schema 是 %d，当前只支持 %d"), b.Schema, evalJSONSchema)
	}
	return b, nil
}

// compareEvalBaseline 找出确定性指标的退化。判据刻意保守：只认「命中变多」「句数
// 明显变多」「新增断言失败」，字数与演绎占比波动大，不拿来判回归。
func compareEvalBaseline(cur, base evalJSON) ([]string, *evalJudgeDelta) {
	byIndex := make(map[int]evalJSONTurn, len(base.Turns))
	for _, t := range base.Turns {
		byIndex[t.Index] = t
	}
	var regs []string
	for _, t := range cur.Turns {
		if t.Err != "" {
			continue
		}
		b, ok := byIndex[t.Index]
		if !ok || b.Err != "" {
			continue
		}
		ids := map[string]bool{}
		for id := range t.HitCounts {
			ids[id] = true
		}
		for id := range b.HitCounts {
			ids[id] = true
		}
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		sort.Strings(list)
		for _, id := range list {
			if t.HitCounts[id] > b.HitCounts[id] {
				regs = append(regs, trf("cli.eval.baseline.hits", "第 %d 轮「%s」命中 %d → %d",
					t.Index, stylecheck.LabelOf(uiLang, id), b.HitCounts[id], t.HitCounts[id]))
			}
		}
		if t.Sentences > b.Sentences+2 {
			regs = append(regs, trf("cli.eval.baseline.sentences", "第 %d 轮句数 %d → %d", t.Index, b.Sentences, t.Sentences))
		}
	}
	var delta *evalJudgeDelta
	if cur.Judge != nil && base.Judge != nil {
		delta = &evalJudgeDelta{
			BaseSame: base.Judge.SamePerson, CurSame: cur.Judge.SamePerson,
			BaseTone: base.Judge.Tone, CurTone: cur.Judge.Tone,
			BaseRelation: base.Judge.Relation, CurRelation: cur.Judge.Relation,
		}
	}
	return regs, delta
}
