package unspoken

import "wen/internal/prompt"

// 两段文本各按语言取一次。
func guideFor(lang string) string { return prompt.T(lang, "prompt.unspoken.guide", guidePromptZH) }
func stateHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.unspoken.state_header", stateHeaderZH)
}

// guidePromptZH 是维护判据。「不直接念出来，但决定态度与潜台词」这一条是本插件存在的理由：
// 嘴上说没事、心里记着的那口气有了落盘的去处，压缩与换会话都接得上。
const guidePromptZH = `[没说出口的话]
你心里有一些没说出口的事：对对方的真实看法、憋着的话、在等的事、决定暂时不提的事。
本轮状态里的[心里话]块就是这些。它们是心里的——不直接念出来，也不写进【】里——
但它们决定你的态度、语气与潜台词：嘴上说没事，心里记着，话就会短半截。
- 只记会在心里留几天的事，用 keep_unspoken 记一条；寻常一轮对话不记，不要每轮都记。
- 心情的起因值得记住时也记一条：心情会回落，起因会丢，这里能留住「为什么不高兴」。
- 说出口了、想通了、不再在意了，用 let_go 放下那一条。
- 这里只记「我心里」的事，不记对方的事实——那归 [对方信息] 与记忆。`

// stateHeaderZH 领起本轮注入的清单。
const stateHeaderZH = "[心里话]"
