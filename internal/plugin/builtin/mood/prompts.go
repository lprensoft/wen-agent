package mood

import "wen/internal/prompt"

// 三段文本各按语言取一次。
func guideFor(lang string) string { return prompt.T(lang, "prompt.mood.guide", guidePromptZH) }
func effectFor(lang string) string {
	return prompt.T(lang, "prompt.mood.effect", effectPromptZH)
}
func stateHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.mood.state_header", stateHeaderZH)
}

// guidePromptZH 是调整判据。「先调整、再演绎」这一条不能省：注入发生在轮次开始，
// 工具回显要到模型写完话才到达，不先调工具的话，被说了重话的那一轮仍然按上一轮
// 的好心情在回应，情绪要迟一拍才落下来。
const guidePromptZH = `[心情]
角色此刻有一个心情值，范围 -100 到 +100，0 为平静，越高越开心、越低越沮丧。
本轮出现了会影响心情的事时——被夸、被冷落、玩笑开过头、久别重逢、被说重话——
先用 adjust_mood 报出变化量，拿到调整后的心情，再据此写这一轮的表达。
- 报的是变化量而不是目标值：好一点是 +10，很难过是 -35。
- 一轮最多调一次，把这一轮的事合起来算，不要每说一句就调一次。
- 只为真正影响到角色的事调整。寻常寒暄、事务性的往来不动心情。
- 没有给出当前心情时按平静起算。
- 心情会随时间自己向平静回落，不需要为「过了一阵子该好些了」专门调整。`

// effectPromptZH 说的是心情怎么落到表达上。插件只给取向，不写台词。
const effectPromptZH = `[心情的作用]
心情影响的是表达方式，不是事实，也不是角色的性格与立场：
- 心情好时话多、主动、容易接话、动作放松；心情差时话短、被动、回应变慢、
  动作收着，也更容易沉默。
- 心情不改变角色是谁。低落的时候仍然是同一个人，只是这个人此刻状态不好。
- 不要把心情当结论说出来（不出现「我现在心情是 -30」这类话，也不报数值），
  让它体现在怎么说话、怎么动作上。
- 心情与此刻正在发生的事冲突时，以正在发生的事为准，并用 adjust_mood 把心情
  调过来，而不是硬按着旧心情演。`

// stateHeaderZH 领起本轮注入的当前心情。
const stateHeaderZH = "[当前心情]"
