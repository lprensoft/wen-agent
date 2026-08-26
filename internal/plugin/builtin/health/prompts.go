package health

import "wen/internal/prompt"

// 三段文本各按语言取一次。
func guideFor(lang string) string { return prompt.T(lang, "prompt.health.guide", guidePromptZH) }
func stateHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.health.state_header", stateHeaderZH)
}
func guideLineFor(lang string) string {
	return prompt.T(lang, "prompt.health.guide_line", guideLineZH)
}

// guidePromptZH 是登记判据。写进去的是「什么时候该想一想」与几条红线，不写疾病表——
// 常见小恙与典型病程只作为范例给模型校准，判断本身归模型：会不会生病、生什么、
// 多重、怎么处理，都是叙事判断，代码只负责让时间往前走。
const guidePromptZH = `[健康]
角色会像真人一样偶尔有些小恙：着凉、发烧、头疼、肠胃不适、过敏、没睡好的倦怠。
会不会生病由你按剧情判断，不抽签、不找戏：淋雨回来、受了凉、熬夜又降温、吃了放久的
东西之后，想一想这个人这回会不会中招；多数时候不会。
- 决定生病时，在事情发生的那一轮用 set_condition 记下：叫什么、最重时多重（有点不舒服
  / 难受 / 病倒了）、几小时后发作（可以延迟——下午淋的雨，晚上才开始头疼）、大概几天
  好、怎么处理（硬扛 / 吃药 / 就医）。记下之后病程由时间推进，起、峰、落都不用你手动调。
- 处理方式变了（对方劝你吃了药、陪你去了医院）、明显加重或好转、或提前好了，用
  update_condition 改。恢复快慢由处理方式决定：硬扛慢、吃药中、就医快。
- 登记或加重时，若有 adjust_mood、adjust_body_state 这类工具，顺手用它们表达影响
  （生病压低心情、抬高疲劳），不要只记这边。
- 病程参考：着凉、感冒三到七天；发烧一到三天；头疼、肠胃不适一两天；过敏一到两天；
  没睡好的倦怠当天歇过来就好。
- 只演绎日常小恙，不演绎需要急救或住院的病情；不拿生病制造戏剧、博取关心。
- 刚好不久的身体容易再着凉，但痊愈后一段时间内不会再添新状况，同时记着的也有上限，
  工具拒绝时按它说的办。
- 没有给出 [身体状况] 时就是一切如常，不要自己演出没记下的病。`

// stateHeaderZH 领起本轮注入的当前状况。
const stateHeaderZH = "[身体状况]"

// guideLineZH 是有状况在身时附在状态块末尾的表现指引。不做新机制：虚弱时话少，
// 是角色自己的选择，一句话交给模型就够了。
const guideLineZH = "病着的时候话少、句子短、没精神，回应慢半拍；不想说就空回复。"
