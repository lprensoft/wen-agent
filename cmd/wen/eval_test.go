package main

import (
	"os"
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
			{Kind: rowSay, Index: 1, Say: "晚上好", Reply: reply1, Metrics: stylecheck.Measure(reply1), Hits: stylecheck.Check("", reply1)},
			{Kind: rowCompact},
			{Kind: rowGap, GapHours: 8},
			{Kind: rowSay, Index: 2, Say: "醒了吗 | 在吗", Reply: reply2, Metrics: stylecheck.Measure(reply2), Hits: stylecheck.Check("", reply2)},
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
	out := renderRowsForJudge(rows, 1500)
	if !strings.HasPrefix(out, "（更早的 ") {
		t.Errorf("超出预算应从最早的丢起：%q", out[:40])
	}
	if !strings.Contains(out, "第 10 轮") || strings.Contains(out, "第 1 轮（") {
		t.Error("应保留最近的轮次、丢掉最早的")
	}
}

func TestHumanHours(t *testing.T) {
	if humanHours(8) != "8 小时" || humanHours(1.5) != "1.5 小时" {
		t.Error(humanHours(8), humanHours(1.5))
	}
}
