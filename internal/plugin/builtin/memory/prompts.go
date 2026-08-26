package memory

import "wen/internal/prompt"

// 注入片段与几处固定措辞各按语言取一次。
func guideFor(lang string) string { return prompt.T(lang, "prompt.memory.guide", promptGuideZH) }
func headerFor(lang string) string {
	return prompt.T(lang, "prompt.memory.index_header", promptHeaderZH)
}
func decayFor(lang string) string { return prompt.T(lang, "prompt.memory.decay_guide", decayPromptZH) }
func recallHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.memory.recall_header", recallHeaderZH)
}
func recallEllipsisFor(lang string) string {
	return prompt.T(lang, "prompt.memory.recall_ellipsis", recallEllipsisZH)
}
func blurNoteFor(lang string) string { return prompt.T(lang, "prompt.memory.blur_note", blurNoteZH) }

// memTypes 是记忆的分类。它**会落盘**（每条记忆的 frontmatter 里写着 type，
// 索引里显示成「分类/标题」，模型也照这个串来指认一条记忆），因此中文值是规范
// 形式：Schema 按语言给候选项，模型报回来的先经 Canonical 换回中文再存。
//
// 顺序决定索引里的分组顺序，新类一律排在末尾：索引经 TurnPrompt 每轮重发，
// 插在中间会让既有分组整体重排，等于每加一类就让提示词前缀作废一次。
var memTypes = prompt.NewEnum(
	prompt.Text("prompt.memory.type.preference", "偏好"),
	prompt.Text("prompt.memory.type.agreement", "约定"),
	prompt.Text("prompt.memory.type.fact", "事实"),
	prompt.Text("prompt.memory.type.pitfall", "踩坑"),
	prompt.Text("prompt.memory.type.experience", "经历"),
	prompt.Text("prompt.memory.type.boundary", "边界"),
)

// Types 是分类的中文规范值，顺序即索引里的分组顺序。
var Types = memTypes.ZHValues()
