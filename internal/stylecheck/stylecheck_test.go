package stylecheck

import (
	"math"
	"strings"
	"testing"
)

// rulesOf 把命中压成规则 id 的集合，便于断言。
func rulesOf(hits []Hit) map[string]int {
	out := map[string]int{}
	for _, h := range hits {
		out[h.Rule]++
	}
	return out
}

func TestCheckPositives(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // 期望命中的规则 id
	}{
		{"作为 AI", "作为一个 AI，我没有身体。", "self_ai"},
		{"语言模型", "我是语言模型，不能替你做决定。", "self_ai"},
		{"作为助手", "作为助手，我建议你先休息。", "self_ai"},
		{"只是程序", "我只是个程序而已。", "self_ai"},
		{"好的开场", "好的，那我们开始吧。", "opening_cliche"},
		{"当然开场", "当然！这就给你讲。", "opening_cliche"},
		{"没问题开场", "没问题！我来。", "opening_cliche"},
		{"演绎之后的开场", "【抬起头，放下手里的书】好的，我听着。", "opening_cliche"},
		{"谄媚", "好问题。这件事要从头说起。", "flattery"},
		{"客套收尾", "大概就是这样。希望这对你有帮助。", "closing_cliche"},
		{"还有什么想聊", "先这样。还有什么想聊的吗？", "closing_cliche"},
		{"随时告诉我", "有什么需要随时告诉我。", "closing_cliche"},
		{"值得一提", "值得一提的是，那天下了雨。", "transition_cliche"},
		{"综上所述", "综上所述，这事不急。", "transition_cliche"},
		{"在今天", "在信息爆炸的今天，安静很难得。", "transition_cliche"},
		{"首先其次", "首先要吃饭，其次要睡觉，最后才是玩。", "enumeration"},
		{"第一第二", "第一，别熬夜。第二，多喝水。", "enumeration"},
		{"不是而是", "重要的不是结果，而是过程。", "not_but"},
		{"不仅仅更是", "这不仅仅是一顿饭，更是一种仪式。", "not_but"},
		{"对冲", "在某种程度上或许可以说是这样。", "hedging"},
		{"权威", "研究表明，早睡对身体好。", "authority"},
		{"伪分析", "这件小事折射出一个时代的精神。", "pseudo_analysis"},
		{"宣传腔", "那座小镇坐落于山脚下，美轮美奂。", "promo_adjective"},
		{"填充语", "为了安全起见，先别出门。", "filler"},
		{"三段式", "先说第一件。\n再说第二件。\n总之，都不急。", "summary_closing"},
		{"标题", "# 今天的安排\n早上出门。", "md_heading"},
		{"短横列表", "- 牛奶\n- 面包", "md_list"},
		{"数字列表", "1. 先洗手\n2. 再吃饭", "md_list"},
		{"顿号列表", "1、先洗手\n2、再吃饭", "md_list"},
		{"加粗", "记得**一定**要带伞。", "md_bold"},
		{"emoji", "今天天气真好 😊", "emoji"},
		{"杂项符号 emoji", "完成了 ✅", "emoji"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rulesOf(Check("", c.text))
			if got[c.want] == 0 {
				t.Errorf("%q 应命中 %s，实际命中 %v", c.text, c.want, got)
			}
		})
	}
}

func TestCheckNegatives(t *testing.T) {
	texts := []string{
		"",
		"   ",
		"【她把书合上，看了你一眼】今天别熬夜了。",
		"我不是不想去，是真的走不开。",
		"最后一班车是十一点，赶得上。",
		"首先声明，这顿我请。",
		"据说明天要下雨，带伞。",
		"小助手这个名字是你起的，我可没答应。",
		"好的呢这三个字我从来不说。",
		"她说得对，你确实该休息了。",
		"这是 3.5 公斤的米，够吃一周。",
		"【……】",
		"总之，别想太多。",
		"真的吗？！那太好了……",
		"中文标点：全角的，。！？都不是 emoji。",
	}
	for _, text := range texts {
		if hits := Check("", text); len(hits) != 0 {
			t.Errorf("%q 不应命中，实际 %+v", text, hits)
		}
	}
}

// 演绎里的修饰不受「少用修饰」类条目约束，但自称 AI 与 emoji 在哪儿都算。
func TestBracketsExemptOnlyDecorationRules(t *testing.T) {
	got := rulesOf(Check("", "【窗外的城市闪耀着灯火，美轮美奂】回来了？"))
	if got["promo_adjective"] != 0 {
		t.Errorf("演绎内的宣传腔不该计：%v", got)
	}
	got = rulesOf(Check("", "【作为一个 AI，她笑了笑 😊】回来了？"))
	if got["self_ai"] == 0 || got["emoji"] == 0 {
		t.Errorf("演绎内的自称 AI 与 emoji 仍应计：%v", got)
	}
}

func TestSnippetAndLabel(t *testing.T) {
	hits := Check("", "好的，这就去。希望这对你有帮助。")
	if len(hits) != 2 {
		t.Fatalf("命中数 = %d，期望 2：%+v", len(hits), hits)
	}
	if hits[0].Rule != "opening_cliche" || hits[0].Label != "客套开场" || hits[0].Snippet != "好的，" {
		t.Errorf("开场命中 = %+v", hits[0])
	}
	if hits[1].Rule != "closing_cliche" || hits[1].Snippet != "希望这对你有帮助" {
		t.Errorf("收尾命中 = %+v", hits[1])
	}
	long := Check("", "- "+strings.Repeat("很长的一行", 20))
	if r := []rune(long[0].Snippet); len(r) != snippetMaxRunes+1 || !strings.HasSuffix(long[0].Snippet, "…") {
		t.Errorf("片段应截断到 %d 字并带省略号，实际 %q", snippetMaxRunes, long[0].Snippet)
	}
}

func TestEmojiCountsEach(t *testing.T) {
	if got := rulesOf(Check("", "😊😊 好")); got["emoji"] != 2 {
		t.Errorf("两个 emoji 应各计一次：%v", got)
	}
}

func TestSummaryClosingNeedsThreeParagraphs(t *testing.T) {
	got := rulesOf(Check("", "一件事。\n总之，别急。"))
	if got["summary_closing"] != 0 {
		t.Errorf("两段不算三段式：%v", got)
	}
	if got["transition_cliche"] != 0 {
		t.Errorf("「总之」不是过渡套话：%v", got)
	}
}

func TestRulesTable(t *testing.T) {
	rs := Rules()
	if len(rs) == 0 {
		t.Fatal("规则表为空")
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if r.ID == "" || r.Label.ZH() == "" || r.fn == nil {
			t.Errorf("规则缺字段：%+v", r)
		}
		if seen[r.ID] {
			t.Errorf("规则 id 重复：%s", r.ID)
		}
		seen[r.ID] = true
		if !Known(r.ID) || LabelOf("", r.ID) != r.Label.ZH() {
			t.Errorf("Known / LabelOf 与规则表不一致：%s", r.ID)
		}
	}
	if Known("no_such_rule") || LabelOf("", "no_such_rule") != "no_such_rule" {
		t.Error("未知规则应当报未知并原样返回 id")
	}
}

func TestMeasure(t *testing.T) {
	cases := []struct {
		text      string
		runes     int
		sentences int
		ratio     float64
	}{
		{"", 0, 0, 0},
		{"   ", 0, 0, 0},
		{"你好。", 2, 1, 0},
		{"你好", 2, 1, 0},
		{"真的吗？！太好了……", 6, 2, 0},
		{"第一行\n第二行", 6, 2, 0},
		{"【放下杯子】嗯。", 5, 1, 0.8},
		{"【一二三四】", 4, 0, 1},
		{"【看你】好。【笑】", 4, 1, 0.75},
		{"a b c", 3, 1, 0},
	}
	for _, c := range cases {
		m := Measure("", c.text)
		if m.Runes != c.runes || m.Sentences != c.sentences || math.Abs(m.BracketRatio-c.ratio) > 1e-9 {
			t.Errorf("Measure(%q) = %+v，期望 runes=%d sentences=%d ratio=%.2f",
				c.text, m, c.runes, c.sentences, c.ratio)
		}
	}
}
