package stylecheck

import (
	"strings"
	"testing"

	"wen/internal/i18n"
)

// ids 把命中折成一组规则 id，方便断言。
func ids(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Rule)
	}
	return out
}

func has(hits []Hit, id string) bool {
	for _, h := range hits {
		if h.Rule == id {
			return true
		}
	}
	return false
}

// TestEnglishRulesFire 逐条确认英文那组模式认得出对应的助手腔。
// 每条都配一句反例，防止规则宽到把正常说话也算进去。
func TestEnglishRulesFire(t *testing.T) {
	for _, c := range []struct {
		id   string
		hit  string
		miss string
	}{
		{"self_ai", "As an AI, I don't really have preferences.", "The AI winter was a long time ago."},
		{"opening_cliche", "Certainly! Here's what I found.", "Sure enough, it rained all afternoon."},
		{"flattery", "Great question — let me think about that.", "She asked a question about the trains."},
		{"closing_cliche", "Hope this helps! Let me know if you need anything.", "That helped a lot, thanks."},
		{"transition_cliche", "It's worth noting that the shop closes early.", "I noted the address down."},
		{"enumeration", "First, we eat. Second, we walk. ", "I got there first."},
		{"not_but", "It's not just a job, it's a whole life.", "It is not raining."},
		{"hedging", "To some extent that's true, I suppose.", "I'm not sure yet."},
		{"authority", "Experts say the market will recover.", "My colleague said the same thing."},
		{"pseudo_analysis", "It's a testament to how much she cared.", "She cared a lot."},
		{"promo_adjective", "Let's delve into the vibrant tapestry of it.", "The soup was good."},
		{"filler", "In order to get there we need to leave now.", "To get there we leave now."},
		{"md_bold", "That was **really** something.", "A star * on its own."},
		{"emoji", "Got home 😮 finally.", "Got home finally."},
		{"em_dash", "I meant it — every word of it.", "I meant every word of it."},
	} {
		t.Run(c.id, func(t *testing.T) {
			if !has(Check(i18n.LangEN, c.hit), c.id) {
				t.Errorf("%q 应当命中 %s，实际命中 %v", c.hit, c.id, ids(Check(i18n.LangEN, c.hit)))
			}
			if has(Check(i18n.LangEN, c.miss), c.id) {
				t.Errorf("%q 不该命中 %s", c.miss, c.id)
			}
		})
	}
}

// TestEnglishSummaryClosing 三段式：至少三段，最后一段以总结词起头。
func TestEnglishSummaryClosing(t *testing.T) {
	text := "There are a few parts to this.\n\nThe first part is easy.\n\nIn summary, it will be fine."
	if !has(Check(i18n.LangEN, text), "summary_closing") {
		t.Errorf("三段式总结没命中:\n%s", text)
	}
	if has(Check(i18n.LangEN, "In summary, it will be fine."), "summary_closing") {
		t.Error("只有一段的总结归过渡套话管，这里不该重复计")
	}
}

// TestChineseRulesUntouchedByEnglishSet 是这次改动的底线：
// 英文那组模式不能让中文回复凭空多出命中。
//
// 挑的都是**在英文规则里会命中、在中文语境下完全正常**的说法：聊代码时说的
// robust 与 leverage、以及夹在拉丁字母之间的破折号。
func TestChineseRulesUntouchedByEnglishSet(t *testing.T) {
	for _, s := range []string{
		"这个方案挺 robust 的，先用着。",
		"我们可以 leverage 一下现有的接口。",
		"文件名是 wen — agent 那个。",
		"first 那个变量名我改了。",
		"【靠在椅背上】今天不想动脑子了。",
	} {
		if hits := Check(i18n.LangZH, s); len(hits) > 0 {
			t.Errorf("中文回复 %q 不该有任何命中，实际 %v", s, ids(hits))
		}
	}
}

// TestActingMarkerPerLanguage 演绎记号按语言认：中文【】，英文 *…*。
// 认错记号的后果是演绎占比恒为 0，而「少用修饰」那几条会连演绎的笔调一起罚。
func TestActingMarkerPerLanguage(t *testing.T) {
	zh := "【靠在椅背上，闭了会儿眼】今天真的不行了。"
	en := "*leans back, eyes closed for a moment* I'm done for today."

	if r := Measure(i18n.LangZH, zh).BracketRatio; r <= 0 {
		t.Errorf("中文的【】没被认成演绎，占比 %v", r)
	}
	if r := Measure(i18n.LangEN, en).BracketRatio; r <= 0 {
		t.Errorf("英文的 *…* 没被认成演绎，占比 %v", r)
	}
	// 中文那条路不认星号：中文角色被要求用的是【】
	if r := Measure(i18n.LangZH, en).BracketRatio; r != 0 {
		t.Errorf("中文那条路不该把 *…* 当演绎，占比 %v", r)
	}
}

// TestBoldIsNotActing **加粗** 里正好含着一对能匹配 *…* 的星号，
// 不先避开的话，每一处加粗都会被当成一段演绎。
func TestBoldIsNotActing(t *testing.T) {
	s := "That was **really** something."
	if r := Measure(i18n.LangEN, s).BracketRatio; r != 0 {
		t.Errorf("**加粗** 不该被当成演绎，占比 %v", r)
	}
	if !has(Check(i18n.LangEN, s), "md_bold") {
		t.Error("加粗本身仍要命中 md_bold")
	}
	// 同一句里两样都有：加粗要认出来，演绎也要认出来
	mixed := "*she looks up* That was **really** something."
	if r := Measure(i18n.LangEN, mixed).BracketRatio; r <= 0 {
		t.Errorf("同一句里的演绎没认出来，占比 %v", r)
	}
}

// TestEnglishSpeechRulesIgnoreActing 「少用修饰」那几条只看话语，不看演绎——
// 与中文那条路的规矩一致。
func TestEnglishSpeechRulesIgnoreActing(t *testing.T) {
	inActing := "*the light is stunning through the window* Morning."
	if has(Check(i18n.LangEN, inActing), "promo_adjective") {
		t.Error("演绎里的 stunning 不该算宣传腔——演绎有自己的笔调")
	}
	inSpeech := "The view is stunning, you should come see it."
	if !has(Check(i18n.LangEN, inSpeech), "promo_adjective") {
		t.Error("话语里的 stunning 应当命中")
	}
}

// TestEnglishOpeningOnlyAtLead 客套开场只看起手，演绎之后的第一句才算。
func TestEnglishOpeningOnlyAtLead(t *testing.T) {
	lead := "*puts the mug down* Sure, I'll take a look."
	if !has(Check(i18n.LangEN, lead), "opening_cliche") {
		t.Error("跳过开头的演绎之后应当认出客套开场")
	}
	mid := "I read it twice. Sure, I'll take a look."
	if has(Check(i18n.LangEN, mid), "opening_cliche") {
		t.Error("句中的 Sure 不算客套开场")
	}
}

// TestEmDashIsChineseSafe 中文的破折号是「——」，且它是正常标点，
// 因此这条规则只在非中文的语言上跑。
func TestEmDashIsChineseSafe(t *testing.T) {
	zh := "他说了一句——那句话我记到现在。"
	if has(Check(i18n.LangZH, zh), "em_dash") {
		t.Error("中文的破折号是正常标点，不该命中")
	}
	if !strings.Contains(zh, "——") {
		t.Fatal("测试用例本身写错了")
	}
}
