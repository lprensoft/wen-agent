package slackbot

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	slackImageRe  = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)\)`)
	slackLinkRe   = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)\)`)
	slackCodeRe   = regexp.MustCompile("`([^`\n]+)`")
	slackBoldRe   = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	slackItalicRe = regexp.MustCompile(`\*([^*\n]+)\*`)
	slackStrikeRe = regexp.MustCompile(`~~([^~\n]+)~~`)
	slackHeaderRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	slackBulletRe = regexp.MustCompile(`^(\s*)[-*+]\s+`)
	slackQuoteRe  = regexp.MustCompile(`^>\s?`)
	slackRuleRe   = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
)

// toSlackMarkdown 把模型输出的标准 markdown 降级成 Slack mrkdwn。Slack 的语法
// 不同（粗体是单星、没有标题、链接是 <url|text>），而且 <@U…> / <!channel> 这类
// 控制序列会真的提到人，所以普通文本里的 < > & 一律转义；我们自己生成的链接用
// 占位符保护，转义完再还原。
func toSlackMarkdown(text string) string {
	var tokens []string
	push := func(s string) string {
		tokens = append(tokens, s)
		return fmt.Sprintf("\x00%d\x00", len(tokens)-1)
	}

	var out []string
	inCode := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
			out = append(out, line)
			continue
		}
		if inCode {
			out = append(out, line)
			continue
		}
		out = append(out, slackLine(line, push))
	}
	s := strings.Join(out, "\n")

	// 占位符按下标逆序还原：粗体/标题是后生成的，里面可能包着链接或行内代码。
	for i := len(tokens) - 1; i >= 0; i-- {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00%d\x00", i), tokens[i])
	}
	return s
}

func slackLine(line string, push func(string) string) string {
	prefix := ""
	if m := slackHeaderRe.FindStringSubmatch(line); m != nil {
		return "*" + slackInline(m[2], push) + "*"
	}
	if slackRuleRe.MatchString(line) {
		return "──────────"
	}
	if strings.HasPrefix(line, ">") {
		prefix = "> "
		line = slackQuoteRe.ReplaceAllString(line, "")
	}
	if m := slackBulletRe.FindStringSubmatch(line); m != nil {
		prefix = m[1] + "• "
		line = slackBulletRe.ReplaceAllString(line, "")
	}
	return prefix + slackInline(line, push)
}

func slackInline(line string, push func(string) string) string {
	line = slackImageRe.ReplaceAllStringFunc(line, func(m string) string {
		sub := slackImageRe.FindStringSubmatch(m)
		return push(slackLink(sub[2], sub[1]))
	})
	line = slackLinkRe.ReplaceAllStringFunc(line, func(m string) string {
		sub := slackLinkRe.FindStringSubmatch(m)
		return push(slackLink(sub[2], sub[1]))
	})
	line = slackCodeRe.ReplaceAllStringFunc(line, func(m string) string {
		return push(m)
	})

	line = escapeSlack(line)
	line = slackBoldRe.ReplaceAllStringFunc(line, func(m string) string {
		sub := slackBoldRe.FindStringSubmatch(m)
		content := sub[1]
		if content == "" {
			content = sub[2]
		}
		return push("*" + content + "*")
	})
	line = slackStrikeRe.ReplaceAllString(line, "~$1~")
	line = slackItalicRe.ReplaceAllString(line, "_$1_")
	return line
}

func slackLink(url, label string) string {
	// URL 里的 & 是查询参数分隔符，不能像正文那样转成 &amp;；只处理会截断
	// Slack 链接语法的字符。
	url = strings.NewReplacer("<", "%3C", ">", "%3E", "|", "%7C").Replace(url)
	label = strings.ReplaceAll(escapeSlack(label), "|", "¦")
	if label == "" || label == url {
		return "<" + url + ">"
	}
	return "<" + url + "|" + label + ">"
}

func escapeSlack(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
