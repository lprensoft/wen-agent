package stylecheck

import "regexp"

// 英文的同一批模式。
//
// 与中文那份逐条对应，但**不是翻译**：中文里暴露机械感的模式（「不是……而是……」
// 「值得一提的是」）在英文里换成了英文自己的那些（em dash 当修辞、delve、
// "It's not just X — it's Y"、开场的 Certainly）。判据与 roleplay 的 [自然表达]
// 一致——那一段是写给模型的要求，这里是同一份要求的检测端。
//
// 两组模式**按语言各跑各的**，不合并成一组同时跑：robust、leverage、first 这些词
// 在中文回复里也会出现（聊代码的时候），合着跑等于让中文用户的统计里凭空多出命中。
// 这次改动的底线是中文那条路一条命中都不多、不少。
var (
	// 自称 AI。这一条与中文那条同时跑（见 rules 表）：它是最硬的回归标记，
	// 中文角色蹦出一句 "as an AI" 同样是出戏。
	selfAIEN = regexp.MustCompile(`(?i)\b(as an?|being an?|i'?m an?|i am an?|i'?m just an?|i am just an?)\s+(a\.i\.|ai|artificial intelligence|(large )?language model|llm|chatbot|virtual assistant|assistant|program)\b`)

	// 客套开场只看起手。要求后面跟标点，避免误伤 "Sure enough" 这类正常句子。
	openingClicheEN = regexp.MustCompile(`(?i)^(sure|certainly|absolutely|of course|got it|understood|no problem|great|happy to help|i'?d be happy to( help)?)\s*[,!.:—…]`)

	flatteryEN = regexp.MustCompile(`(?i)((that'?s|what) an? (great|excellent|interesting|fascinating|wonderful) (question|point|idea)|great question|excellent question|you'?re absolutely right|i'?m glad you asked|good catch)`)

	closingClicheEN = regexp.MustCompile(`(?i)(hope (this|that) helps|let me know if|feel free to (ask|reach out|let me know)|if you have any (other |further |more )?questions|is there anything else|anything else (i can help|you'?d like)|don'?t hesitate to (ask|reach out)|happy to (help|assist) (further|more))`)

	transitionClicheEN = regexp.MustCompile(`(?i)(it'?s worth noting|it is worth noting|that being said|that said,|in (summary|conclusion)|to sum up|overall,|moreover,|furthermore,|in other words|needless to say|as (we all know|you know),|in today'?s (world|age|landscape)|let'?s (dive|delve|explore|take a look))`)

	// 「不是 X 而是 Y」的两种英文写法：显式的 not just … but，以及 It's not X — it's Y。
	notButEN = regexp.MustCompile(`(?i)(\bnot (just|merely|simply|only)\b[^.!?\n]{1,40}?\bbut\b|\bit'?s not\b[^.!?\n]{1,30}?[—,]\s*it'?s\b)`)

	hedgingEN = regexp.MustCompile(`(?i)(to some extent|in a sense|it could be argued|one could argue|arguably,|perhaps maybe|maybe possibly|more or less|somewhat of a)`)

	authorityEN = regexp.MustCompile(`(?i)(experts? (say|believe|suggest|agree|note)|studies (show|suggest|indicate)|research (shows|suggests|indicates)|scientists (found|believe)|it is (widely )?believed|data shows)`)

	pseudoAnalysisEN = regexp.MustCompile(`(?i)(a testament to|speaks to the|reflects a (deeper|broader)|underscores the|highlights the (importance|significance)|serves as a reminder|embodies the)`)

	// 宣传腔与那批被用滥的词。delve / leverage / robust 这类在技术讨论里是正常用词，
	// 所以它们只在英文那条路上跑，且这条规则本来就只看话语、不看演绎。
	promoAdjectiveEN = regexp.MustCompile(`(?i)\b(stunning|breathtaking|nestled|tapestry|unparalleled|delve|leverage|robust|seamless|game-?changer|cutting-?edge|state-of-the-art|meticulously|realm of|myriad)\b`)

	fillerEN = regexp.MustCompile(`(?i)(in order to|due to the fact that|at this point in time|for all intents and purposes|the fact of the matter is|it is important to note that)`)

	summaryLeadEN = regexp.MustCompile(`(?i)^(in (summary|conclusion)|to sum up|overall|all in all|in short|ultimately|to conclude|in essence)\b`)

	// em dash 当修辞。中文的破折号是「——」（两个），且中文里它是正常标点，
	// 因此这条只认**拉丁字母之间**的单个 —，中文正文一句都不会命中。
	emDashEN = regexp.MustCompile(`[A-Za-z][ \t]*—[ \t]*[A-Za-z]`)

	// 成对出现才算给对话编号，与中文那条的判据一致。
	firstEN   = regexp.MustCompile(`(?i)\bfirst(ly)?\b`)
	secondEN  = regexp.MustCompile(`(?i)\bsecond(ly)?\b`)
	finallyEN = regexp.MustCompile(`(?i)\b(finally|lastly|third(ly)?)\b`)
)

// summaryLeadFor 按语言给出「最后一段是不是在做总结」的起手词。
func summaryLeadFor(lang string) *regexp.Regexp {
	if isZH(lang) {
		return summaryLead
	}
	return summaryLeadEN
}

// enumerationEN 认 first 配上 second / finally / lastly——与中文那条同一个判据：
// 单独一个 first 在口语里很常见，成对出现才是在给对话编号。
func enumerationEN(s string) []string {
	first := firstEN.FindString(s)
	if first == "" {
		return nil
	}
	for _, re := range []*regexp.Regexp{secondEN, finallyEN} {
		if m := re.FindString(s); m != "" {
			return []string{first + "…" + m}
		}
	}
	return nil
}
