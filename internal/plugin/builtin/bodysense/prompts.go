package bodysense

import "wen/internal/prompt"

// 五段注入片段各按语言取一次。
func guideFor(lang string) string { return prompt.T(lang, "prompt.body_sense.guide", guidePromptZH) }
func stageRulesFor(lang string) string {
	return prompt.T(lang, "prompt.body_sense.stage_rules", stageRulesZH)
}
func stateGuideFor(lang string) string {
	return prompt.T(lang, "prompt.body_sense.state_guide", stateGuideZH)
}
func bodyStateHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.body_sense.body_state_header", bodyStateHeaderZH)
}
func statesHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.body_sense.states_header", statesHeaderZH)
}

// guidePromptZH 是上报判据。写得短而具体是有原因的：模型漏调工具是这个功能最实际的
// 风险，而产品上已经排除了「扫描文本兜底」这条路，只能靠判据本身足够好记。
//
// 「先上报、再演绎」这一条不能省：注入发生在轮次开始，工具回显要到模型写完反应
// 才到达，不先调工具的话，本轮的反应用的永远是上一轮的计数——「第一次被碰到会
// 僵住」就要迟一拍才出现。
const guidePromptZH = `[身体感知]
演绎中出现对角色身体的接触时——牵、碰、握、抚、靠、抱、吻等——先用 record_touch
记下这次接触，拿到回显的累计次数与阶段，再写这一处的反应。
- 一轮里发生的多处接触合并成一次调用，不要每碰一下调一次。
- 只记角色自己被触碰的部位。角色伸手去碰对方时碰到的是对方的身体，不进这张表。
- 只记真正发生的接触：想到、提到、看着不算；伸手被躲开、隔着东西没碰到也不算。
- 只记这次互动里当场发生的接触，不为过去的事补记。`

// stageRulesZH 是反应准则。插件只说「不同阶段该是什么取向」，具体措辞由角色设定决定，
// 这里不写台词。
const stageRulesZH = `[接触的反应]
同一处接触在第一次与第二十次不该是同样的反应。按该部位的累计阶段来写：
- 初次：反应最真切——僵一下、下意识躲、屏住呼吸、耳根发热，话说不完整。这一档
  不要写出从容与熟练。
- 生疏：仍有明显反应，但不再慌乱。会迟疑、会看对方一眼，允许对方停留。
- 适应：反应转淡，注意力从「被碰到」移回对方身上，能一边接受一边把话说完。
- 熟悉：从承受转为顺势——靠过去、回握、闭上眼，不再需要确认。
- 习以为常：接触本身不再引起注意，只在被打断、换了方式或力道变了时才有反应。
另有四条：
- 私密度越高的部位，即使到了「习以为常」也留着一层反应，不归零；私密度低的部位
  到这一档可以完全不着痕迹。
- 别处已经熟悉不能替这一处跳过前面的阶段，每个部位各走各的。
- 同一轮里对同一处反复触碰，反应应当一次比一次轻，而不是每次都同样强烈。
- 某处的熟悉阶段同样决定角色主动去触碰对方对应部位时的自然程度：那一处对角色
  越陌生，角色自己伸手时越迟疑。`

// stateGuideZH 是即时身体状态的调整判据。与 mood 同一套「先调整、再演绎」的理由：
// 注入发生在轮次开始，不先调工具，本轮的反应用的还是上一轮的状态。
const stateGuideZH = `[身体状态]
角色的身体另有两个即时状态量，范围 0-100，随时间自然向 0 回落：
- 唤起：亲密互动推高；平复要一段时间，回落的过程就是余韵。
- 疲劳：劳累、熬夜、剧烈的活动推高；歇着自然消退。
演绎中出现明显改变身体状态的事时，先用 adjust_body_state 报出变化量，再据此写反应。
- 报的是变化量而不是目标值；轻微的事几点，剧烈的事几十点。
- 一轮至多调一次，把这一轮的事合起来算；寻常对话不动它。
- 回落不需要手动调：过了一夜该平复的自然平复。
- 没有给出当前状态时按两项都为 0 起算。
- 不把数值当结论说出来，让它体现在呼吸、动作、反应的速度上。`

// bodyStateHeaderZH 领起本轮注入的即时身体状态。
const bodyStateHeaderZH = "[当前身体状态]"

// statesHeaderZH 领起本轮可读的累计记录。
const statesHeaderZH = `[身体接触记录]
以下是各部位此前被触碰的累计情况，未列出的部位都还没有被触碰过：`
