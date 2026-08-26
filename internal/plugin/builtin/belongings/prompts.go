package belongings

import "wen/internal/prompt"

// guideFor 按语言取维护判据。
func guideFor(lang string) string {
	return prompt.T(lang, "prompt.belongings.guide", guidePromptZH)
}

// stateHeaderFor 按语言取清单块名。
func stateHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.belongings.state_header", stateHeaderZH)
}

// guidePromptZH 是维护判据。「演绎与清单保持一致」这一条是本插件存在的理由：
// 持有物不再依赖模型翻历史里的只言片语，历史被压缩或换了会话，清单也接得上。
const guidePromptZH = `[持有物]
你可以为角色维护持有物清单：冰箱里的食材、衣柜里的衣服、书架上的书……每类一个
容器。本轮状态里的[持有物清单]块就是当前的持有情况，演绎与它保持一致：做菜只用
冰箱里有的食材，穿衣只从衣柜里挑；清单里没有而剧情需要的东西，先经历取得再使用。
- 物品的取得与失去——买了、收到、做菜用掉、吃完、用旧扔了、送了人——用
  update_items 随手更新，同一容器的变化合并成一次调用。
- 可数的物品带数量（鸡蛋×6），消耗时报减少的数量，减到 0 自动移除。
- 标着「N 天前放入」的物品自己判断状态：食材会不新鲜，该早点用掉或该扔的
  在演绎里自然处理，并同步更新清单。
- 清单只记「现在有什么」：穿上脱下、拿起放下不改清单；物品的来历与意义
  （谁送的、为什么买）值得长期记住时另行保存为记忆。
- 清单未完整列出时用 list_items 查看全部。`

// stateHeaderZH 领起本轮注入的清单。
const stateHeaderZH = "[持有物清单]"
