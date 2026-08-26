package statustext

import (
	"testing"
	"unicode"

	"wen/internal/i18n"
	"wen/internal/plugin"
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

// 状态命令的措辞三处输出共用（Web UI 另有一份在浏览器里），漏一句就会在英文用户的
// QQ 或微信里冒出一行中文。逐条键有没有译文是覆盖测试的事，这里查的是拼出来的整段。
func TestEnglishStatusHasNoChinese(t *testing.T) {
	info := plugin.StatusInfo{
		Version: "v0.0.0", Provider: "p", Model: "m", Thinking: "high",
		ContextLength: 200000, HasSession: true, MessageCount: 12,
		MeasuredTokens: 3000, EstTokens: 2500,
		CachedTokens: 1000, CacheWriteTokens: 200, PromptTokens: 2000,
	}
	for _, s := range []string{
		Render(i18n.LangEN, info, "s1"),
		Render(i18n.LangEN, plugin.StatusInfo{Version: "v0.0.0", Provider: "p", Model: "m", Thinking: "high"}, ""),
		CacheLine(i18n.LangEN, info),
	} {
		if han := hanIn(s); han != "" {
			t.Errorf("英文状态输出里有中文 %q:\n%s", han, s)
		}
	}
}
