package plugin

import (
	"regexp"
	"strings"
)

// StripHTML 把一段 HTML 粗剥成可读文本：去掉 script/style 与标签，解几个常见实体，
// 压缩空白。
//
// 它不追求解析正确。调用方要的是「别把标签与脚本塞进上下文」，而不是还原页面结构：
// 认真解析要一个 HTML5 词法分析器，那是另一个体量的依赖，而剥剩下的残渣模型读得懂。
//
// 放在这里而不是某个插件里，是因为 web_fetch 与 web_search 都要用它。两份实现迟早会
// 在「哪个实体记得解」上分叉，而分叉的表现是英文界面里冒出 `&amp;` 这种残渣。
func StripHTML(s string) string {
	s = reScript.ReplaceAllString(s, " ")
	// 常见块级标签换成换行，避免文本黏连
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	s = reSpaces.ReplaceAllString(s, " ")
	// 去掉每行首尾空白后压缩连续空行
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	return reBlank.ReplaceAllString(s, "\n\n")
}

var (
	reScript = regexp.MustCompile(`(?is)<(script|style|noscript)\b.*?</\s*(script|style|noscript)\s*>`)
	reBlock  = regexp.MustCompile(`(?i)</\s*(p|div|li|h[1-6]|tr|br)\s*>|<br\s*/?>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlank  = regexp.MustCompile(`\n{3,}`)
	reSpaces = regexp.MustCompile(`[ \t]{2,}`)
)
