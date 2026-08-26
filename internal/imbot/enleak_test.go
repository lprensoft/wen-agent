package imbot

import (
	"testing"
	"unicode"

	"wen/internal/i18n"
)

func hanIn(s string) string {
	var out []rune
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			out = append(out, r)
		}
	}
	return string(out)
}

// 通道骨架的两处文本：命令帮助是给人看的（i18n），聊天节奏的提示词是发给模型的
// （prompt）。两份语言包各有各的覆盖测试，这里查的是拼出来的整段。
func TestEnglishSkeletonTextHasNoChinese(t *testing.T) {
	for _, s := range []string{
		HelpText(i18n.LangEN),
		ChatStylePrompt(i18n.LangEN),
		ThinkingLine(i18n.LangEN, "thinking"),
		ToolsLine(i18n.LangEN, []string{"save_memory"}),
	} {
		if han := hanIn(s); han != "" {
			t.Errorf("英文输出里有中文 %q:\n%s", han, s)
		}
	}
}
