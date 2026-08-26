// Package stylecheck 是一组纯函数的「助手腔」检测规则：给一段回复文本，返回其中
// 命中了哪些机械表达模式，以及字数、句数、【】演绎占比这几项基础度量。
//
// 规则逐条对应 roleplay 插件 [自然表达] 段落里的禁止项——那一段是写给模型的
// 要求，这里是同一份要求的检测端：提示词说「不要这样写」，检测器回答「有没有
// 这样写」。此前全库没有任何生成后处理，角色有没有漂只能凭感觉，每次调提示词都
// 是盲调；有了数字才谈得上「先测量，后干预」。
//
// 本包零模型调用、零 I/O、无状态，可以放在任何热路径上。检测是启发式的：命中
// 不等于一定出了戏（「不是……而是……」也是正常中文会说的话），它给的是趋势
// 而不是判决，用法是按天累计、看比例的变化。
package stylecheck

import (
	"regexp"
	"strings"

	"wen/internal/prompt"
)

// Hit 是一次规则命中。
type Hit struct {
	Rule    string // 规则 id，稳定，可用于配置忽略与统计键
	Label   string // 规则名，已按调用方的语言取出
	Snippet string // 命中的那一小段原文（已截断）
}

// Rule 描述一条检测规则。
type Rule struct {
	ID string
	// Label 是这条规则的名字。它进文风注记与状态行（给人看），也进 style_report
	// 的输出（给模型看），两处都跟随进程语言，因此放提示词语言包。
	Label prompt.Piece
	// fn 在预处理后的文本上找出全部命中片段；返回空表示未命中。
	fn func(t prepared) []string
}

// Rules 返回全部规则（按固定顺序），供配置界面列举与忽略项校验。
func Rules() []Rule {
	out := make([]Rule, len(rules))
	copy(out, rules)
	return out
}

// LabelOf 返回规则 id 的中文标签；未知 id 原样返回。
func LabelOf(lang, id string) string {
	for _, r := range rules {
		if r.ID == id {
			return r.Label.For(lang)
		}
	}
	return id
}

// Known 报告 id 是否为已知规则。
func Known(id string) bool {
	for _, r := range rules {
		if r.ID == id {
			return true
		}
	}
	return false
}

// snippetMaxRunes 限制命中片段的长度：它会被拼进注记与报告，一眼认得出就够。
const snippetMaxRunes = 24

// Check 在文本上跑全部规则，按规则顺序返回命中；同一规则命中多处返回多条。
func Check(lang, text string) []Hit {
	t := prepare(lang, text)
	if t.full == "" {
		return nil
	}
	var hits []Hit
	for _, r := range rules {
		for _, s := range r.fn(t) {
			hits = append(hits, Hit{Rule: r.ID, Label: r.Label.For(lang), Snippet: clip(s)})
		}
	}
	return hits
}

// prepared 是预处理后的文本的几种视图，各规则按需取用。
type prepared struct {
	lang   string // 本轮的语言：决定演绎用哪个记号，也决定用哪一组模式
	full   string // 原文（去首尾空白）
	speech string // 去掉全部演绎后的文字：话语与叙述
	lead   string // 去掉开头连续的演绎之后的起手部分
	paras  []string
}

func prepare(lang, text string) prepared {
	full := strings.TrimSpace(text)
	t := prepared{lang: lang, full: full}
	t.speech = strings.TrimSpace(cutSpans(full, actingSpans(lang, full), " "))
	lead := full
	for {
		lead = strings.TrimLeft(lead, " \t\r\n　")
		spans := actingSpans(lang, lead)
		if len(spans) == 0 || spans[0][0] != 0 {
			break
		}
		lead = lead[spans[0][1]:]
	}
	t.lead = lead
	for _, p := range strings.Split(full, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			t.paras = append(t.paras, p)
		}
	}
	return t
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= snippetMaxRunes {
		return s
	}
	return string(r[:snippetMaxRunes]) + "…"
}

// ---------- 规则表 ----------

// 规则的顺序就是命中的输出顺序，也是配置界面列举的顺序：先是最硬的回归标记
// （自称 AI），再是开场与收尾的客套，然后是句式与结构，最后是排版。

var rules = []Rule{
	{ID: "self_ai", Label: prompt.Text("prompt.style.rule.self_ai", "自称 AI"), fn: anyOf(selfAI, selfAIEN)},
	{ID: "opening_cliche", Label: prompt.Text("prompt.style.rule.opening_cliche", "客套开场"), fn: leadMatch(byLang(openingCliche, openingClicheEN))},
	{ID: "flattery", Label: prompt.Text("prompt.style.rule.flattery", "谄媚开场"), fn: regexAll(byLang(flattery, flatteryEN))},
	{ID: "closing_cliche", Label: prompt.Text("prompt.style.rule.closing_cliche", "客套收尾"), fn: regexAll(byLang(closingCliche, closingClicheEN))},
	{ID: "transition_cliche", Label: prompt.Text("prompt.style.rule.transition_cliche", "过渡套话"), fn: regexAll(byLang(transitionCliche, transitionClicheEN))},
	{ID: "enumeration", Label: prompt.Text("prompt.style.rule.enumeration", "首先其次"), fn: enumeration},
	{ID: "not_but", Label: prompt.Text("prompt.style.rule.not_but", "不是而是"), fn: regexAll(byLang(notBut, notButEN))},
	{ID: "hedging", Label: prompt.Text("prompt.style.rule.hedging", "过度对冲"), fn: regexAll(byLang(hedging, hedgingEN))},
	{ID: "authority", Label: prompt.Text("prompt.style.rule.authority", "泛指权威"), fn: regexAll(byLang(authority, authorityEN))},
	{ID: "pseudo_analysis", Label: prompt.Text("prompt.style.rule.pseudo_analysis", "伪分析"), fn: speechRegexAll(byLang(pseudoAnalysis, pseudoAnalysisEN))},
	{ID: "promo_adjective", Label: prompt.Text("prompt.style.rule.promo_adjective", "宣传腔"), fn: speechRegexAll(byLang(promoAdjective, promoAdjectiveEN))},
	{ID: "filler", Label: prompt.Text("prompt.style.rule.filler", "填充语"), fn: regexAll(byLang(filler, fillerEN))},
	{ID: "summary_closing", Label: prompt.Text("prompt.style.rule.summary_closing", "三段式总结"), fn: summaryClosing},
	{ID: "md_heading", Label: prompt.Text("prompt.style.rule.md_heading", "标题"), fn: lineMatch(only(mdHeading))},
	{ID: "md_list", Label: prompt.Text("prompt.style.rule.md_list", "列表"), fn: lineMatch(only(mdList))},
	{ID: "md_bold", Label: prompt.Text("prompt.style.rule.md_bold", "加粗"), fn: regexAll(only(mdBold))},
	{ID: "emoji", Label: prompt.Text("prompt.style.rule.emoji", "emoji"), fn: emoji},
	// em dash 当修辞只在英文里算毛病：中文的破折号是两个「——」，且它本来就是
	// 正常标点。这条因此只在非中文的语言上跑，且只认拉丁字母之间的单个 —。
	{ID: "em_dash", Label: prompt.Text("prompt.style.rule.em_dash", "破折号当修辞"), fn: enOnly(regexAll(only(emDashEN)))},
}

var (
	// 自称 AI：「作为（一个/一名）AI / 人工智能 / 语言模型 / 助手」「我是（个）AI」
	// 「我只是个程序」。「助手」单独出现太常见（「小助手」「助手小姐」），只认
	// 带「作为」「我是」「我只是」引出的自我定位。
	selfAI = regexp.MustCompile(`(作为|我是|我只是|我毕竟是|身为)(一个|一名|个|一款)?\s?(AI|ai|人工智能|大?语言模型|智能助手|AI\s?助手|虚拟助手|助手|聊天机器人|机器人|程序)`)

	// 客套开场只看起手：「好的，」「当然！」「没问题！」这类应答腔。
	openingCliche = regexp.MustCompile(`^(好的|当然|没问题|当然可以|明白了|收到|了解|好的呢)[，,！!。.～~：:]`)

	flattery = regexp.MustCompile(`(好问题|问得好|说得太对了|你说得很对|你的想法很有意思|这个问题很有意思|很高兴你问)`)

	closingCliche = regexp.MustCompile(`(希望(这|以上|这些|这个回答)?(能|对你|对您)?有(所)?帮助|还有什么(想聊|想问|想说|需要|问题)的?吗|有什么需要(随时|尽管|都可以)?(告诉我|找我|说)|如果(有|还有)(其他|别的|任何|什么)(问题|需要)|随时(告诉我|找我|联系我|来问我)|期待(你|您)的(回复|回音)|祝(你|您)(生活|工作|一切)?(愉快|顺利))`)

	transitionCliche = regexp.MustCompile(`(值得一提的是|总的来说|总体来说|总体而言|综上所述|换句话说|换言之|让我们|需要注意的是|不得不说|在[^，。！？\n]{1,12}的今天|众所周知|毋庸置疑|不可否认)`)

	notBut = regexp.MustCompile(`(不是[^。！？\n；;]{1,30}?而是|不仅仅?是[^。！？\n；;]{1,30}?更是|不只是[^。！？\n；;]{1,30}?更是|不单是[^。！？\n；;]{1,30}?更是)`)

	hedging = regexp.MustCompile(`(可能也许|也许可能|也许大概|大概也许|在某种程度上|某种意义上|或许可以说|可以说是某种|一定程度上来说)`)

	// 「据说」不收：它在口语里太常见（「据说明天下雨」），收进来就是在惩罚正常说话。
	authority = regexp.MustCompile(`(专家(认为|表示|指出|建议)|研究(表明|显示|发现|证明)|有人说|数据(显示|表明)|科学家(发现|认为)|心理学(认为|上说))`)

	pseudoAnalysis = regexp.MustCompile(`(体现着|折射出|彰显着|彰显了|映射出|象征着|昭示着|诠释着|承载着[^，。]{0,6}(意义|精神|情怀))`)

	promoAdjective = regexp.MustCompile(`(令人惊叹|美轮美奂|坐落于|闪耀着|独一无二|叹为观止|无与伦比|举世闻名|璀璨夺目|熠熠生辉|令人陶醉)`)

	filler = regexp.MustCompile(`(为了[^，。！？\n]{1,10}起见|由于[^，。！？\n]{1,12}的原因|出于[^，。！？\n]{1,10}的考虑)`)

	summaryLead = regexp.MustCompile(`^(总之|总的来说|总而言之|综上所述|综上|总结一下|总结来说|归根结底|归根到底|简而言之)`)

	mdHeading = regexp.MustCompile(`^#{1,6}\s+\S`)
	mdList    = regexp.MustCompile(`^(\s*[-*•]\s+\S|\s*\d{1,2}[.、．)）]\s*\S)`)
	mdBold    = regexp.MustCompile(`\*\*[^*\n]+\*\*`)
)

// picker 按本轮的语言挑一条正则。
type picker func(prepared) *regexp.Regexp

// byLang 中文一条、其余语言一条。
//
// 两组模式各跑各的，不合并同跑：robust、leverage、first 这些词在中文回复里也会
// 出现（聊代码的时候），合着跑等于让中文用户的统计里凭空多出命中。
func byLang(zh, other *regexp.Regexp) picker {
	return func(t prepared) *regexp.Regexp {
		if isZH(t.lang) {
			return zh
		}
		return other
	}
}

// only 是不分语言的那一档：同一条正则在所有语言上跑（排版类的规则都是这样）。
func only(re *regexp.Regexp) picker {
	return func(prepared) *regexp.Regexp { return re }
}

// regexAll 在全文上找出全部匹配。
func regexAll(pick picker) func(prepared) []string {
	return func(t prepared) []string { return pick(t).FindAllString(t.full, -1) }
}

// speechRegexAll 只看话语与叙述，不看演绎：[自然表达] 明说演绎有自己的笔调，
// 「少用修饰」这类条目不管它。
func speechRegexAll(pick picker) func(prepared) []string {
	return func(t prepared) []string { return pick(t).FindAllString(t.speech, -1) }
}

// leadMatch 只看起手：跳过开头的演绎之后的第一句。
func leadMatch(pick picker) func(prepared) []string {
	return func(t prepared) []string {
		if m := pick(t).FindString(t.lead); m != "" {
			return []string{m}
		}
		return nil
	}
}

// anyOf 把几条正则一起跑，命中合并。给「不分语言都算毛病」的那种规则用——
// 目前只有自称 AI：中文角色蹦出一句 "as an AI" 同样是出戏。
func anyOf(res ...*regexp.Regexp) func(prepared) []string {
	return func(t prepared) []string {
		var out []string
		for _, re := range res {
			out = append(out, re.FindAllString(t.full, -1)...)
		}
		return out
	}
}

// enOnly 让一条规则只在非中文的语言上跑。
func enOnly(fn func(prepared) []string) func(prepared) []string {
	return func(t prepared) []string {
		if isZH(t.lang) {
			return nil
		}
		return fn(t)
	}
}

// lineMatch 逐行匹配，命中的行作为片段。
func lineMatch(pick picker) func(prepared) []string {
	return func(t prepared) []string {
		var out []string
		re := pick(t)
		for _, line := range strings.Split(t.full, "\n") {
			if re.MatchString(line) {
				out = append(out, strings.TrimSpace(line))
			}
		}
		return out
	}
}

// enumeration 认「首先」配上「其次 / 然后 / 最后」，或「第一」配「第二」——
// 单独一个「首先」「最后」在口语里很常见，成对出现才是在给对话编号。
func enumeration(t prepared) []string {
	if !isZH(t.lang) {
		return enumerationEN(t.full)
	}
	s := t.full
	switch {
	case strings.Contains(s, "首先") && (strings.Contains(s, "其次") || strings.Contains(s, "最后") || strings.Contains(s, "再者")):
		return []string{"首先…" + pick(s, "其次", "最后", "再者")}
	case strings.Contains(s, "第一，") && strings.Contains(s, "第二，"),
		strings.Contains(s, "第一、") && strings.Contains(s, "第二、"),
		strings.Contains(s, "一是") && strings.Contains(s, "二是"):
		return []string{"第一…第二…"}
	}
	return nil
}

func pick(s string, words ...string) string {
	for _, w := range words {
		if strings.Contains(s, w) {
			return w
		}
	}
	return ""
}

// summaryClosing 认三段式：至少三段，且最后一段以总结词起头。只有一段的
// 「总之……」归过渡套话那条管，这里不重复计。
func summaryClosing(t prepared) []string {
	if len(t.paras) < 3 {
		return nil
	}
	last := t.paras[len(t.paras)-1]
	last = strings.TrimSpace(cutSpans(last, actingSpans(t.lang, last), ""))
	if m := summaryLeadFor(t.lang).FindString(last); m != "" {
		return []string{last}
	}
	return nil
}

// emoji 按 Unicode 区段数：表情符号区、杂项符号与装饰符号区、区域指示符（旗帜）。
// 中文标点与全角符号不在这些区段里，不会误伤。每个 emoji 一条命中。
func emoji(t prepared) []string {
	var out []string
	for _, r := range t.full {
		if isEmoji(r) {
			out = append(out, string(r))
		}
	}
	return out
}

func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF: // 表情、交通、补充符号与象形文字
		return true
	case r >= 0x1F000 && r <= 0x1F2FF: // 麻将、扑克、带框字母数字
		return true
	case r >= 0x2600 && r <= 0x27BF: // 杂项符号与装饰符号（☀ ✨ ❤ ✅）
		return true
	case r == 0x2B50 || r == 0x2B55 || r == 0x203C || r == 0x2049:
		return true
	}
	return false
}
