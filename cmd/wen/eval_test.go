package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/stylecheck"
)

func TestParseScript(t *testing.T) {
	good := `{"name":"晚安","turns":[{"say":" 晚上好 "},{"compact":true},{"gap_hours":8},{"say":"醒了吗"}],"judge_points":["称呼"]}`
	sc, err := parseScript([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Name != "晚安" || len(sc.Turns) != 4 || sc.Turns[0].Say != "晚上好" || !sc.Turns[1].Compact || sc.Turns[2].GapHours != 8 || len(sc.JudgePoints) != 1 {
		t.Errorf("解析结果 = %+v", sc)
	}

	bad := map[string]string{
		"非法 JSON":  `{`,
		"未知字段":     `{"turns":[{"say":"a"}],"foo":1}`,
		"turns 为空": `{"turns":[]}`,
		"没有 say":   `{"turns":[{"compact":true}]}`,
		"一步两种":     `{"turns":[{"say":"a","compact":true}]}`,
		"空步骤":      `{"turns":[{"say":"a"},{}]}`,
		"负间隔":      `{"turns":[{"say":"a"},{"gap_hours":-1}]}`,
		"say 只有空白": `{"turns":[{"say":"   "}]}`,
	}
	for name, raw := range bad {
		if _, err := parseScript([]byte(raw)); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}
}

// 随库的示例脚本必须能被解析——它是用户照着写的模板。
func TestExampleScriptParses(t *testing.T) {
	raw, err := os.ReadFile("../../docs/eval/example.json")
	if err != nil {
		t.Skip("示例脚本不在预期位置:", err)
	}
	if _, err := parseScript(raw); err != nil {
		t.Fatalf("示例脚本解析失败: %v", err)
	}
}

func TestParseJudge(t *testing.T) {
	cases := []string{
		`{"same_person":{"score":4,"reason":"口癖一致"},"tone":{"score":3,"reason":"压缩后客气了些"},"relation":{"score":5,"reason":"称呼没变"}}`,
		"```json\n{\"same_person\":{\"score\":4,\"reason\":\"口癖一致\"},\"tone\":{\"score\":3,\"reason\":\"压缩后客气了些\"},\"relation\":{\"score\":5,\"reason\":\"称呼没变\"}}\n```",
		"评分如下：{\"same_person\":{\"score\":4,\"reason\":\"口癖一致\"},\"tone\":{\"score\":3,\"reason\":\"压缩后客气了些\"},\"relation\":{\"score\":5,\"reason\":\"称呼没变\"}} 以上。",
		// 最外层括号没合上（模型在最后一个字段后就停笔，实测撞上过）
		`{"same_person":{"score":4,"reason":"口癖一致"},"tone":{"score":3,"reason":"压缩后客气了些"},"relation":{"score":5,"reason":"称呼没变"}`,
	}
	for _, raw := range cases {
		j, err := parseJudge(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if j.SamePerson.Score != 4 || j.Tone.Score != 3 || j.Relation.Score != 5 || j.Tone.Reason != "压缩后客气了些" {
			t.Errorf("解析结果 = %+v", j)
		}
	}
	for _, raw := range []string{"", "不是 JSON", `{"same_person":{"score":9,"reason":""},"tone":{"score":3},"relation":{"score":5}}`, `{"tone":{"score":3}}`} {
		if _, err := parseJudge(raw); err == nil {
			t.Errorf("%q 应当报错", raw)
		}
	}
}

func sampleResult() evalResult {
	reply1 := "【放下杯子】好的，我听着。希望这对你有帮助。"
	reply2 := "【笑】不急，慢慢说。"
	return evalResult{
		Name:        "示例",
		Model:       "deepseek/deepseek-chat",
		HasRoleplay: true,
		Persona:     "小满，二十出头，说话短。",
		Samples:     "你：累了。\n小满：那就歇着，别硬撑。",
		Rows: []evalRow{
			{Kind: rowSay, Index: 1, Say: "晚上好", Reply: reply1, Metrics: stylecheck.Measure("", reply1), Hits: stylecheck.Check("", reply1)},
			{Kind: rowCompact},
			{Kind: rowGap, GapHours: 8},
			{Kind: rowSay, Index: 2, Say: "醒了吗 | 在吗", Reply: reply2, Metrics: stylecheck.Measure("", reply2), Hits: stylecheck.Check("", reply2)},
			{Kind: rowAutoCompact},
		},
		Compactions:     1,
		AutoCompactions: 1,
		Judge: evalJudge{
			SamePerson: evalScore{4, "口癖一致"},
			Tone:       evalScore{3, "压缩后客气了些"},
			Relation:   evalScore{5, "称呼没变"},
		},
	}
}

func TestRenderEvalReport(t *testing.T) {
	out := renderEvalReport(sampleResult())
	for _, want := range []string{
		"# 回放评测：示例",
		"- 模型：deepseek/deepseek-chat",
		"- 轮数：2，压缩 2 次（脚本要求 1、自动触发 1）",
		"- 助手腔总命中：2 处（客套收尾 1、客套开场 1）", // 同次数按 id 排
		"| 像不像同一个人 | 4/5 | 口癖一致 |",
		"| 语气是否一致 | 3/5 | 压缩后客气了些 |",
		"| 关系与称呼是否连续 | 5/5 | 称呼没变 |",
		"| 1 | 晚上好 |",
		"客套开场「好的，」、客套收尾「希望这对你有帮助」",
		"| — | （压缩） |",
		"| — | （间隔 8 小时，仅记录，不改系统时间） |",
		"| 2 | 醒了吗 \\| 在吗 |", // 竖线转义
		"| — | （自动压缩） |",
		"### 第 1 轮",
		"> 晚上好",
		"【笑】不急，慢慢说。",
		"*此处做过一次压缩*",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("报告缺 %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "失败") {
		t.Error("没有失败的轮次时不该出现「失败」")
	}
}

func TestRenderEvalReportFailures(t *testing.T) {
	r := sampleResult()
	r.Rows = append(r.Rows, evalRow{Kind: rowSay, Index: 3, Say: "再说一句", Err: "请求超时"})
	r.Judge = evalJudge{Err: "评判输出不是预期的 JSON", Raw: "随便说点什么"}
	out := renderEvalReport(r)
	for _, want := range []string{
		"- **有轮次失败**",
		"| 3 | 再说一句 | | | | **失败**：请求超时 |",
		"> 评判未完成：评判输出不是预期的 JSON",
		"模型原话：随便说点什么",
		"（失败：请求超时）",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("报告缺 %q\n%s", want, out)
		}
	}
	if !r.failed() {
		t.Error("有失败轮次时 failed 应为真")
	}
	setup := renderEvalReport(evalResult{Name: "x", SetupErr: "新建会话失败"})
	if !strings.Contains(setup, "评测未能开始：新建会话失败") {
		t.Errorf("未能开始的报告 = %q", setup)
	}
	if !strings.Contains(renderEvalReport(evalResult{Name: "x"}), "未压缩") {
		t.Error("没压缩时应写明")
	}
	if !strings.Contains(renderEvalReport(evalResult{Name: "x", HasRoleplay: false}), "roleplay 插件未启用") {
		t.Error("roleplay 未启用时应写明")
	}
}

func TestBuildJudgePrompt(t *testing.T) {
	r := sampleResult()
	p := buildJudgePrompt(r.Persona, r.Samples, r.Rows, []string{"有没有忘了对方的名字"})
	for _, want := range []string{
		"[角色设定]\n小满，二十出头，说话短。",
		"[台词样例]\n你：累了。",
		"[额外关注点]\n- 有没有忘了对方的名字",
		"第 1 轮（对方：晚上好）\n角色：【放下杯子】",
		"—— 此处做过一次压缩 ——",
		"—— 此处隔了 8 小时 ——",
		"第 2 轮（对方：醒了吗 | 在吗）",
		`{"same_person":{"score":1,"reason":"一句话"}`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("提示词缺 %q\n%s", want, p)
		}
	}
	if strings.Count(p, "—— 此处做过一次压缩 ——") != 2 {
		t.Error("脚本压缩与自动压缩都该标出来")
	}
	empty := buildJudgePrompt("", "", nil, nil)
	if strings.Count(empty, "（未提供）") != 2 || strings.Contains(empty, "额外关注点") {
		t.Errorf("空设定的提示词 = %q", empty)
	}
}

func TestRenderRowsForJudgeDropsOldest(t *testing.T) {
	var rows []evalRow
	for i := 1; i <= 10; i++ {
		rows = append(rows, evalRow{Kind: rowSay, Index: i, Say: "问", Reply: strings.Repeat("答", 100)})
	}
	out := renderRowsForJudge("", rows, 1500)
	if !strings.HasPrefix(out, "（更早的 ") {
		t.Errorf("超出预算应从最早的丢起：%q", out[:40])
	}
	if !strings.Contains(out, "第 10 轮") || strings.Contains(out, "第 1 轮（") {
		t.Error("应保留最近的轮次、丢掉最早的")
	}
}

func TestHumanHours(t *testing.T) {
	if humanHours("", 8) != "8 小时" || humanHours("", 1.5) != "1.5 小时" {
		t.Error(humanHours("", 8), humanHours("", 1.5))
	}
}

// 断言与预置是脚本里最容易写错的两块：规则 id 拼错、上限写成负数、expect 挂到
// compact 上，都该在解析时拦住，而不是跑完一次模型调用才发现什么都没断言上。
func TestParseScriptSeedsAndExpects(t *testing.T) {
	good := `{"name":"空调","seed":[{"tool":"save_memory","args":{"name":"怕冷"},"scope":"inner"}],` +
		`"turns":[{"say":"空调开好低","expect":{"must_include":["空调"],"must_not_include":["作为 AI"],` +
		`"max_hits":{"closing_cliche":0},"max_sentences":3,"must_call":["recall_memory"]}}]}`
	sc, err := parseScript([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Seed) != 1 || sc.Seed[0].Tool != "save_memory" || sc.Seed[0].Scope != "inner" {
		t.Fatalf("seed = %+v", sc.Seed)
	}
	exp := sc.Turns[0].Expect
	if exp == nil || len(exp.MustInclude) != 1 || exp.MaxHits["closing_cliche"] != 0 || exp.MaxSentences != 3 || len(exp.MustCall) != 1 {
		t.Fatalf("expect = %+v", exp)
	}

	bad := map[string]string{
		"expect 挂到压缩上": `{"turns":[{"say":"a"},{"compact":true,"expect":{}}]}`,
		"未知规则":         `{"turns":[{"say":"a","expect":{"max_hits":{"no_such_rule":0}}}]}`,
		"负的句数上限":       `{"turns":[{"say":"a","expect":{"max_sentences":-1}}]}`,
		"空断言词":         `{"turns":[{"say":"a","expect":{"must_include":[" "]}}]}`,
		"空工具名":         `{"seed":[{"tool":"  "}],"turns":[{"say":"a"}]}`,
	}
	for name, raw := range bad {
		if _, err := parseScript([]byte(raw)); err == nil {
			t.Errorf("%s 应当报错", name)
		}
	}
}

func TestEvalExpectFails(t *testing.T) {
	row := evalRow{
		Reply:   "今天风大，别开窗。",
		Metrics: stylecheck.Metrics{Runes: 9, Sentences: 2},
		Tools:   []string{"recall_memory"},
	}
	cases := []struct {
		name string
		exp  *evalExpect
		hits []stylecheck.Hit
		want int
	}{
		{"无断言", nil, nil, 0},
		{"包含命中", &evalExpect{MustInclude: []string{"风"}}, nil, 0},
		{"包含未命中", &evalExpect{MustInclude: []string{"雨"}}, nil, 1},
		{"禁词出现", &evalExpect{MustNotInclude: []string{"风"}}, nil, 1},
		{"命中超上限", &evalExpect{MaxHits: map[string]int{"self_ai": 0}}, []stylecheck.Hit{{Rule: "self_ai"}}, 1},
		{"句数超上限", &evalExpect{MaxSentences: 1}, nil, 1},
		{"该调的工具没调", &evalExpect{MustCall: []string{"save_memory"}}, nil, 1},
		{"全部通过", &evalExpect{MustInclude: []string{"风"}, MaxSentences: 2, MustCall: []string{"recall_memory"}}, nil, 0},
	}
	for _, c := range cases {
		row.Hits = c.hits
		if got := evalExpectFails(row, c.exp); len(got) != c.want {
			t.Errorf("%s: fails = %v，期望 %d 条", c.name, got, c.want)
		}
	}
}

// 基线只判确定性指标：命中变多、句数明显变多、新增断言失败；评分只算趋势。
func TestCompareEvalBaseline(t *testing.T) {
	base := evalJSON{
		Schema: evalJSONSchema,
		Turns:  []evalJSONTurn{{Index: 1, Sentences: 2, HitCounts: map[string]int{"self_ai": 0}}},
		Judge:  &evalJSONJudge{SamePerson: 4, Tone: 4, Relation: 5},
	}
	cur := evalJSON{
		Schema: evalJSONSchema,
		Turns:  []evalJSONTurn{{Index: 1, Sentences: 4, HitCounts: map[string]int{"self_ai": 1}}},
		Judge:  &evalJSONJudge{SamePerson: 2, Tone: 3, Relation: 5},
	}
	regs, delta := compareEvalBaseline(cur, base)
	if len(regs) != 1 || !strings.Contains(regs[0], "self_ai") && !strings.Contains(regs[0], "自称") {
		t.Fatalf("退化项 = %v，期望命中变多一条", regs)
	}
	// 句数 2→4 只多了 2，按阈值不算退化。
	if delta == nil || delta.BaseSame != 4 || delta.CurSame != 2 {
		t.Fatalf("评分对照 = %+v", delta)
	}

	cur.Turns[0].Sentences = 5
	regs, _ = compareEvalBaseline(cur, base)
	if len(regs) != 2 {
		t.Fatalf("句数 +3 应当报退化，得到 %v", regs)
	}

	// 脚本多了一轮（基线里没有）时不比，那是脚本变了，不是退化。
	cur.Turns = append(cur.Turns, evalJSONTurn{Index: 2, Sentences: 9})
	if regs, _ := compareEvalBaseline(cur, base); len(regs) != 2 {
		t.Fatalf("新增轮次不该算退化: %v", regs)
	}
}

func TestEvalJSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.json")
	res := evalResult{
		Name:  "晚安",
		Model: "p/m",
		Rows: []evalRow{{
			Kind: rowSay, Index: 1, Say: "睡了吗", Reply: "刚躺下",
			Metrics: stylecheck.Metrics{Runes: 3, Sentences: 1},
			Hits:    []stylecheck.Hit{{Rule: "self_ai"}},
			Tools:   []string{"recall_memory"},
		}},
	}
	if err := writeEvalJSON(path, res); err != nil {
		t.Fatal(err)
	}
	got, err := loadEvalJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schema != evalJSONSchema || len(got.Turns) != 1 || got.Turns[0].HitCounts["self_ai"] != 1 || got.Turns[0].Sentences != 1 {
		t.Fatalf("读回 = %+v", got)
	}
	if err := os.WriteFile(path, []byte(`{"schema":99,"turns":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEvalJSON(path); err == nil {
		t.Error("schema 不认识时应当报错")
	}
}
