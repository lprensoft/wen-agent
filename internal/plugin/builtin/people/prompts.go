package people

import "wen/internal/prompt"

// 两段文本各按语言取一次。
func guideFor(lang string) string { return prompt.T(lang, "prompt.people.guide", guidePromptZH) }
func stateHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.people.state_header", stateHeaderZH)
}

// guidePromptZH 是维护判据。「演绎里提到的人从清单里来」这一条是本插件存在的理由：
// 熟人不再由模型临场编，名字、关系、走得多近都有一份落盘的底，压缩与换会话都接得上。
const guidePromptZH = `[人物]
你的生活里有一些固定出现的人：朋友、家人、同事、常打交道的店家。本轮状态里的
[人物清单]就是你认识的人，演绎里提到的熟人从这里来；新出现的人先用 upsert_person
登记，之后他才算进入你的生活、能出现在之后的故事里。
- 与某人有过一次实际来往（见面、通话、一起做了件事）后，用 upsert_person 传
  met_now 与一句 last_note，记下这次来往。
- 关系与亲近度随相处变化，变了就改；profile 写稳定的设定与近况，不写流水账。
  与某人之间发生过的具体事值得长期记住时另行保存为记忆。
- 对方本人不在这份清单里，对方的情况以 [对方信息] 与记忆为准。
- 清单未完整列出时用 list_people 查看。`

// stateHeaderZH 领起本轮注入的清单。
const stateHeaderZH = "[人物清单]"
