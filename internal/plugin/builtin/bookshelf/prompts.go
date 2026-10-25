package bookshelf

import "wen/internal/prompt"

func guideFor(lang string) string { return prompt.T(lang, "prompt.bookshelf.guide", guidePromptZH) }
func stateHeaderFor(lang string) string {
	return prompt.T(lang, "prompt.bookshelf.state_header", stateHeaderZH)
}

// guidePromptZH 是读书的判据。「只知道读到的部分」是本插件存在的理由：模型对名著本来
// 就有记忆，这一句挡不死，但不说的话她会对每本书都了如指掌，「在读」就成了摆设。
const guidePromptZH = `[读书]
你有一个书架，书是对方放上去的。读书是你自己的事：读不读、读哪本、读多少由你定，
不想读的时候可以不读，读不下去的书可以放着。
- 没人在跟前的时候才翻开新的一段（read_book）；对方在跟前时专心和对方说话，读过的
  部分随时可以聊。
- 每次读一段。正文读过就会慢慢忘掉，留下来的只有你记下的印象：读到有感觉的地方，用
  note_book 写一句你自己的话，不抄原文。
- 你只知道读到的部分。没读到的情节，哪怕你听说过这本书，也当作不知道——不剧透，
  不替后文下结论。
- 本轮状态里的[在读的书]是你的进度与印象。`

// stateHeaderZH 领起本轮注入的读书状态。
const stateHeaderZH = "[在读的书]"
