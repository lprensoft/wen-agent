// Package mdtext 提供把模型输出的 markdown 转成可读纯文本、以及按消息上限
// 分段的纯函数，供不渲染（或只部分渲染）markdown 的远程 IM 通道共用
// （QQ 的纯文本降级路径、微信等）。
package mdtext

import (
	"regexp"
	"strings"
	"wen/internal/i18n"
)

var (
	imageRe   = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)\)`)
	boldRe    = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	strikeRe  = regexp.MustCompile(`~~([^~]+)~~`)
	italicRe  = regexp.MustCompile(`\*([^*\n]+)\*`) // 下划线斜体不处理，避免误伤 snake_case
	inlineRe  = regexp.MustCompile("`([^`\n]+)`")
	linkRe    = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)\)`)
	headerRe  = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	bulletRe  = regexp.MustCompile(`^(\s*)[-*+]\s+`)
	quoteRe   = regexp.MustCompile(`^>\s?`)
	hrRe      = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
	escapeRe  = regexp.MustCompile(`\\([\\*_#` + "`" + `\[\]()~>])`)
	blank3Re  = regexp.MustCompile(`\n{3,}`)
	codeFence = "──────────"
)

// ToPlainText 把 markdown 压成可读纯文本：加粗/斜体/删除线剥标记、行内代码
// 加「」、标题加【】、列表换 •、引用换 ｜、链接展开为「文字（URL）」、代码块
// 围栏换分隔线且内容全程保护不参与行内替换。表格不在此处理——各通道对表格的
// 降级策略不同（QQ 有专门的键值行降级），调用方自行前置。
func ToPlainText(lang, text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	inCode := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
			out = append(out, codeFence)
			continue
		}
		if inCode {
			out = append(out, line)
			continue
		}
		line = imageRe.ReplaceAllString(line, i18n.T(lang, "mdtext.image", "[图片]")+" $2")
		line = linkRe.ReplaceAllStringFunc(line, func(m string) string {
			sub := linkRe.FindStringSubmatch(m)
			if sub[1] == "" || sub[1] == sub[2] {
				return sub[2]
			}
			return sub[1] + "（" + sub[2] + "）"
		})
		line = boldRe.ReplaceAllString(line, "$1$2")
		line = strikeRe.ReplaceAllString(line, "$1")
		line = italicRe.ReplaceAllString(line, "$1")
		line = inlineRe.ReplaceAllString(line, "「$1」")
		if m := headerRe.FindStringSubmatch(line); m != nil {
			line = "【" + m[2] + "】"
		}
		line = bulletRe.ReplaceAllString(line, "$1• ")
		line = quoteRe.ReplaceAllString(line, "｜ ")
		if hrRe.MatchString(line) {
			line = codeFence
		}
		line = escapeRe.ReplaceAllString(line, "$1")
		out = append(out, line)
	}
	return strings.TrimSpace(blank3Re.ReplaceAllString(strings.Join(out, "\n"), "\n\n"))
}

// ChunkMarkdown 按行把文本切成不超过 limit 个 rune 的分段，且保证 markdown 结构完整：
// 代码块中途要断开时先补 ``` 闭合、下一段以原围栏行重开（否则第二段整体变成代码/
// 代码整体变成正文）；超长单行硬切（不能整体保留，平台会拒收超限消息）。
// 纯文本输入同样适用（没有围栏时退化为按行分段）。
func ChunkMarkdown(s string, limit int) []string {
	var chunks []string
	var cur []string
	curLen := 0
	inCode := false
	fence := "```"

	flush := func() {
		if len(cur) == 0 {
			return
		}
		text := strings.TrimSpace(strings.Join(cur, "\n"))
		if text != "" {
			chunks = append(chunks, text)
		}
		cur, curLen = nil, 0
	}
	appendLine := func(line string) {
		cur = append(cur, line)
		curLen += len([]rune(line)) + 1
	}

	for _, line := range strings.Split(s, "\n") {
		lineRunes := []rune(line)
		// 超长单行：先冲掉当前段，再按 limit 硬切成独立段
		if len(lineRunes) > limit {
			if inCode {
				appendLine(fence)
			}
			flush()
			for i := 0; i < len(lineRunes); i += limit {
				end := min(i+limit, len(lineRunes))
				chunks = append(chunks, string(lineRunes[i:end]))
			}
			if inCode {
				appendLine(fence)
			}
			continue
		}
		if curLen+len(lineRunes)+1 > limit {
			if inCode {
				appendLine("```") // 闭合本段的代码块
				flush()
				appendLine(fence) // 下一段重开同款围栏
			} else {
				flush()
			}
		}
		appendLine(line)
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !inCode {
				fence = strings.TrimSpace(line)
			}
			inCode = !inCode
		}
	}
	flush()
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	return chunks
}
