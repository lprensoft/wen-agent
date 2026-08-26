package roleplay

import (
	"strings"

	"wen/internal/llm"
	"wen/internal/textclip"
)

// maxSceneRunes 限制回写进摘要的场景长度：注记会长期留在会话历史里。
const maxSceneRunes = 300

// clipTriple 让三段文本共用一个字节预算，截断顺序为 设定 > 我的信息 > 样例。
// 样例排最末，因为它最可降级——没有它角色仍然成立，只是味道淡。
func clipTriple(lang, first, second, samples string, limit int) (string, string, string) {
	first, second = textclip.ClipPair(lang, first, second, limit)
	return first, second, textclip.ClipSegments(lang, samples, limit-len(first)-len(second))
}

// lastScene 从历史中取出最后一处【】里的内容（不含括号本身）。
// 只看助手消息：用户也可能用【】，但场景的连续性是由扮演方维持的。
func lastScene(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != llm.RoleAssistant {
			continue
		}
		if s := lastBracketed(history[i].Content); s != "" {
			return truncateRunes(s, maxSceneRunes)
		}
	}
	return ""
}

// lastBracketed 返回文本中最后一对【】之间的内容。
func lastBracketed(s string) string {
	end := strings.LastIndex(s, "】")
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(s[:end], "【")
	if start < 0 {
		return ""
	}
	return strings.TrimSpace(s[start+len("【") : end])
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// 近期原话的体量上限：几句足以锚定声音，多了就是把刚压掉的历史又塞回去。
const (
	maxVoiceLines = 3
	maxVoiceRunes = 120
)

// recentVoice 取历史末尾最多 maxVoiceLines 条助手原话（按时间顺序），去掉【】演绎只留
// 说出口的话——场景另有注记，这里要的是腔调。空的（只有工具调用或只有演绎）跳过。
func recentVoice(history []llm.Message) []string {
	var lines []string
	for i := len(history) - 1; i >= 0 && len(lines) < maxVoiceLines; i-- {
		if history[i].Role != llm.RoleAssistant {
			continue
		}
		line := strings.Join(strings.Fields(stripBracketed(history[i].Content)), " ")
		if line == "" {
			continue
		}
		lines = append(lines, truncateRunes(line, maxVoiceRunes))
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return lines
}

// stripBracketed 去掉文本中所有【】段（含括号）。不配对的括号原样保留。
func stripBracketed(s string) string {
	var b strings.Builder
	for {
		start := strings.Index(s, "【")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], "】")
		if end < 0 {
			break
		}
		b.WriteString(s[:start])
		s = s[start+end+len("】"):]
	}
	b.WriteString(s)
	return b.String()
}

// joinQuoted 把几句话用「」各自括起来连成一串。
func joinQuoted(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("「")
		b.WriteString(l)
		b.WriteString("」")
	}
	return b.String()
}
