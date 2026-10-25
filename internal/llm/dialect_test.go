package llm

import (
	"strings"
	"testing"
)

// 各方言拼出的思考参数。
func TestApplyThinking(t *testing.T) {
	req := func(thinking string) ChatRequest {
		return ChatRequest{Thinking: thinking, Temperature: 0.7}
	}
	build := func(dialect, thinking string) wireRequest {
		var wr wireRequest
		applyThinking(&wr, req(thinking), dialect)
		return wr
	}

	// DeepSeek（默认）：enabled/disabled + effort，思考态不发 temperature
	wr := build("", "high")
	if wr.Thinking == nil || wr.Thinking.Type != "enabled" || wr.ReasoningEffort != "high" || wr.Temperature != nil {
		t.Fatalf("deepseek 思考态: %+v", wr)
	}
	wr = build(DialectDeepSeek, "off")
	if wr.Thinking == nil || wr.Thinking.Type != "disabled" || wr.Temperature == nil {
		t.Fatalf("deepseek 关闭态: %+v", wr)
	}

	// MiniMax：adaptive/disabled + reasoning_split，temperature 照发，不发 effort
	wr = build(DialectMiniMax, "high")
	if wr.Thinking == nil || wr.Thinking.Type != "adaptive" || !wr.ReasoningSplit ||
		wr.ReasoningEffort != "" || wr.Temperature == nil {
		t.Fatalf("minimax 思考态: %+v", wr)
	}
	// MiniMax 关不掉思考：选「关闭」也照开着发（M3.1-Flash 收到 disabled 会 400）
	wr = build(DialectMiniMax, "off")
	if wr.Thinking == nil || wr.Thinking.Type != "adaptive" || !wr.ReasoningSplit || wr.Temperature == nil {
		t.Fatalf("minimax 关闭态应照开着发: %+v", wr)
	}

	// Qwen：enable_thinking 布尔
	wr = build(DialectQwen, "medium")
	if wr.EnableThinking == nil || !*wr.EnableThinking || wr.Thinking != nil {
		t.Fatalf("qwen 思考态: %+v", wr)
	}
	wr = build(DialectQwen, "off")
	if wr.EnableThinking == nil || *wr.EnableThinking {
		t.Fatalf("qwen 关闭态: %+v", wr)
	}

	// effort：仅 reasoning_effort，思考态不发 temperature
	wr = build(DialectEffort, "low")
	if wr.ReasoningEffort != "low" || wr.Thinking != nil || wr.Temperature != nil {
		t.Fatalf("effort 思考态: %+v", wr)
	}
	wr = build(DialectEffort, "off")
	if wr.ReasoningEffort != "" || wr.Temperature == nil {
		t.Fatalf("effort 关闭态: %+v", wr)
	}

	// none：什么思考参数都不发
	wr = build(DialectNone, "high")
	if wr.Thinking != nil || wr.ReasoningEffort != "" || wr.EnableThinking != nil || wr.Temperature == nil {
		t.Fatalf("none: %+v", wr)
	}
}

// <think> 剥离：含标签被流式切碎的情况。
func TestThinkFilter(t *testing.T) {
	feedAll := func(chunks []string) (content, reasoning string) {
		f := &thinkFilter{}
		var c, r strings.Builder
		for _, ch := range chunks {
			cc, rr := f.feed(ch)
			c.WriteString(cc)
			r.WriteString(rr)
		}
		cc, rr := f.flush()
		c.WriteString(cc)
		r.WriteString(rr)
		return c.String(), r.String()
	}

	// 完整标签在同一 chunk
	c, r := feedAll([]string{"<think>思考中</think>答案"})
	if c != "答案" || r != "思考中" {
		t.Fatalf("整段: content=%q reasoning=%q", c, r)
	}

	// 标签被切碎在多个 chunk
	c, r = feedAll([]string{"<th", "ink>分", "析</thi", "nk>结", "论"})
	if c != "结论" || r != "分析" {
		t.Fatalf("切碎: content=%q reasoning=%q", c, r)
	}

	// 没有标签：原样通过
	c, r = feedAll([]string{"普通", "回答"})
	if c != "普通回答" || r != "" {
		t.Fatalf("无标签: content=%q reasoning=%q", c, r)
	}

	// 未闭合的思考：flush 后归入思考
	c, r = feedAll([]string{"<think>只有思考没有闭合"})
	if c != "" || r != "只有思考没有闭合" {
		t.Fatalf("未闭合: content=%q reasoning=%q", c, r)
	}

	// 形似标签前缀但不是标签：flush 时按原文吐回正文
	c, r = feedAll([]string{"结尾是 <thi"})
	if c != "结尾是 <thi" || r != "" {
		t.Fatalf("假前缀: content=%q reasoning=%q", c, r)
	}
}

// 档位收敛：界面上那六档取自 Anthropic，别家没一家全收，
// 原样发出去的结果是一个 400，而错误文案来自对方服务器。
func TestSnapEffortRoundsUp(t *testing.T) {
	lhm := []string{"low", "high", "max"}    // 智谱 GLM / Kimi
	lmh := []string{"low", "medium", "high"} // Gemini

	cases := []struct {
		levels []string
		want   string
		got    string
	}{
		{lhm, "low", snapEffort(lhm, "low")},
		{lhm, "high", snapEffort(lhm, "medium")}, // 与 DeepSeek 服务端的归并一致
		{lhm, "max", snapEffort(lhm, "xhigh")},
		{lhm, "max", snapEffort(lhm, "max")},
		{lmh, "medium", snapEffort(lmh, "medium")},
		{lmh, "high", snapEffort(lmh, "xhigh")}, // 没有更高的就取最高档
		{lmh, "high", snapEffort(lmh, "max")},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("snapEffort(%v) = %q，想要 %q", c.levels, c.got, c.want)
		}
	}
	// 不带档位的方言什么都不发
	if got := snapEffort(nil, "high"); got != "" {
		t.Errorf("无档位方言不该给出 effort，得到 %q", got)
	}
}

// 各家方言实际发出去的档位。盯的是「不会发出对方不认识的取值」。
func TestDialectEffortIsAccepted(t *testing.T) {
	for _, name := range KnownDialects {
		spec := dialectSpecs[name]
		if len(spec.levels) == 0 {
			continue
		}
		for _, want := range []string{"low", "medium", "high", "xhigh", "max"} {
			var wr wireRequest
			applyThinking(&wr, ChatRequest{Thinking: want, Temperature: 0.7}, name)
			if wr.ReasoningEffort == "" {
				t.Errorf("方言 %s 在 %s 档下没发 effort", name, want)
				continue
			}
			if !contains(spec.levels, wr.ReasoningEffort) {
				t.Errorf("方言 %s 在 %s 档下发出了对方不接受的 %q（只认 %v）",
					name, want, wr.ReasoningEffort, spec.levels)
			}
		}
	}
}

// OpenAI 系有两条硬限制，漏一条就是个 400：拒绝 temperature，
// 且在 /chat/completions 上只接受 max_completion_tokens。
func TestOpenAIDialectAvoidsRejectedParams(t *testing.T) {
	for _, thinking := range []string{"off", "low", "max"} {
		wr := buildRequest(ChatRequest{
			Model: "gpt-5.6-terra", Thinking: thinking, Temperature: 0.7, MaxTokens: 4096,
		}, DialectOpenAI)
		if wr.Temperature != nil {
			t.Errorf("thinking=%s 时仍发了 temperature", thinking)
		}
		if wr.MaxTokens != 0 {
			t.Errorf("thinking=%s 时发了 max_tokens，OpenAI 只认 max_completion_tokens", thinking)
		}
		if wr.MaxCompletionTokens != 4096 {
			t.Errorf("thinking=%s 时 max_completion_tokens = %d，想要 4096", thinking, wr.MaxCompletionTokens)
		}
	}
	// 关闭思考走 none 这一档，而不是靠不发 effort 再补个 temperature
	var wr wireRequest
	applyThinking(&wr, ChatRequest{Thinking: "off"}, DialectOpenAI)
	if wr.ReasoningEffort != "none" {
		t.Errorf("OpenAI 关闭思考应发 effort=none，得到 %q", wr.ReasoningEffort)
	}
}

// 别家仍然用 max_tokens —— 这条是为了防止上面那个改动漏到所有方言身上。
func TestOtherDialectsKeepMaxTokens(t *testing.T) {
	for _, name := range []string{"", DialectDeepSeek, DialectGLM, DialectQwen, DialectNone} {
		wr := buildRequest(ChatRequest{Model: "m", Thinking: "high", MaxTokens: 2048}, name)
		if wr.MaxTokens != 2048 || wr.MaxCompletionTokens != 0 {
			t.Errorf("方言 %q 的输出上限写错了: max_tokens=%d max_completion_tokens=%d",
				name, wr.MaxTokens, wr.MaxCompletionTokens)
		}
	}
}

// MiniMax 与 Kimi 已把 max_tokens 标为废弃，改发 max_completion_tokens。
func TestDeprecatedMaxTokensDialects(t *testing.T) {
	for _, name := range []string{DialectMiniMax, DialectKimi} {
		wr := buildRequest(ChatRequest{Model: "m", Thinking: "high", MaxTokens: 2048}, name)
		if wr.MaxCompletionTokens != 2048 || wr.MaxTokens != 0 {
			t.Errorf("方言 %q 应发 max_completion_tokens: max_tokens=%d max_completion_tokens=%d",
				name, wr.MaxTokens, wr.MaxCompletionTokens)
		}
	}
}

// Kimi K3 的思考关不掉：「关闭」落到最低一档 low，而不是什么都不发（那样等于默认的 max）；
// 采样参数是固定值，任何情况下都不发。
func TestKimiOffAndSampling(t *testing.T) {
	for _, thinking := range []string{"off", "high"} {
		var wr wireRequest
		applyThinking(&wr, ChatRequest{Thinking: thinking, Temperature: 0.7}, DialectKimi)
		if wr.Temperature != nil {
			t.Errorf("thinking=%s 时 Kimi 不该发 temperature", thinking)
		}
		if thinking == "off" && wr.ReasoningEffort != "low" {
			t.Errorf("Kimi 关闭思考应发 effort=low，得到 %q", wr.ReasoningEffort)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
