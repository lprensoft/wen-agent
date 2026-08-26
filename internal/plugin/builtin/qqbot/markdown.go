package qqbot

import (
	"regexp"
	"strings"

	"wen/internal/i18n"
)

// QQ 的原生 markdown 只支持一个有限子集（加粗/斜体/链接/行内代码/代码块/引用/
// 列表/标题），GFM 表格与图片不渲染（官方文档声称支持表格，但腾讯自家的两个
// 参考实现都实测降级了——以实践为准）。发送前先把不支持的语法降级掉。

// sanitizeForQQ 是 markdown 方言降级管线，顺序固定：表格 → 图片 → 长代码块
// （表格必须在图片之前，否则单元格里的图片语法会干扰行匹配）。
func sanitizeForQQ(lang, text string) string {
	return truncateLongCode(lang, downgradeImages(lang, downgradeTables(lang, text)))
}

var (
	tableRowRe = regexp.MustCompile(`^\s*\|(.+)\|\s*$`)
	tableSepRe = regexp.MustCompile(`^\s*\|?[\s:|-]+\|?\s*$`)
	imageRe    = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)\)`)
)

// maxTableRows 表格降级后保留的数据行数，其余折叠。
const maxTableRows = 3

// downgradeTables 把 GFM 表格降级成「表头: 值 ｜ 表头: 值」的键值行。
// QQ 不渲染表格，纯文本下管道对齐也必崩（无等宽字体），键值列表是唯一可读的形态。
func downgradeTables(lang, text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	var block []string
	inCode := false

	flush := func() {
		if len(block) == 0 {
			return
		}
		rows := make([][]string, 0, len(block))
		for _, line := range block {
			if tableSepRe.MatchString(line) {
				continue // 丢弃 |---|---| 分隔行
			}
			m := tableRowRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			var cells []string
			for _, c := range strings.Split(m[1], "|") {
				cells = append(cells, strings.ReplaceAll(strings.TrimSpace(c), `\|`, "|"))
			}
			rows = append(rows, cells)
		}
		total := len(rows) // 折叠提示要在截断前取长度
		if total == 0 {
			block = nil
			return
		}
		headers := rows[0]
		data := rows[1:]
		if len(data) > maxTableRows {
			data = data[:maxTableRows]
		}
		if len(data) == 0 {
			out = append(out, strings.Join(headers, " / "))
		}
		for _, row := range data {
			var pairs []string
			for i, cell := range row {
				h := ""
				if i < len(headers) {
					h = headers[i]
				}
				if cell == "" {
					continue // 空值对不输出
				}
				pairs = append(pairs, h+": "+cell)
			}
			if len(pairs) > 0 {
				out = append(out, strings.Join(pairs, " ｜ "))
			}
		}
		if hidden := total - 1 - len(data); hidden > 0 {
			out = append(out, i18n.Tf(lang, "plugin.qq_bot.md.table_folded",
				"…（另有 %d 行数据，表格已折叠）", hidden))
		}
		block = nil
	}

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			flush()
			inCode = !inCode
			out = append(out, line)
			continue
		}
		if !inCode && tableRowRe.MatchString(line) {
			block = append(block, line)
			continue
		}
		flush()
		out = append(out, line)
	}
	flush()
	return strings.Join(out, "\n")
}

// downgradeImages 把行内图片降级为普通链接（QQ 不渲染 markdown 图片）。
func downgradeImages(lang, text string) string {
	return imageRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := imageRe.FindStringSubmatch(m)
		alt := sub[1]
		if alt == "" {
			alt = i18n.T(lang, "plugin.qq_bot.md.image", "图片")
		}
		return "[" + alt + "](" + sub[2] + ")"
	})
}

// maxCodeRunes 单个代码块的内容上限，超出截断（回退到最近的换行处）。
const maxCodeRunes = 1200

func truncateLongCode(lang, text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	var code []string
	codeLen := 0
	inCode := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !inCode {
				inCode = true
				out = append(out, line)
				code, codeLen = nil, 0
				continue
			}
			inCode = false
			body := strings.Join(code, "\n")
			if r := []rune(body); len(r) > maxCodeRunes {
				cut := string(r[:maxCodeRunes])
				if i := strings.LastIndexByte(cut, '\n'); i > maxCodeRunes/2 {
					cut = cut[:i]
				}
				body = cut + i18n.Tf(lang, "plugin.qq_bot.md.code_truncated",
					"\n…（代码过长已截断，共 %d 字符）", len(r))
			}
			out = append(out, body, line)
			continue
		}
		if inCode {
			code = append(code, line)
			codeLen += len([]rune(line))
			continue
		}
		out = append(out, line)
	}
	if inCode { // 未闭合的代码块原样保留
		out = append(out, code...)
	}
	return strings.Join(out, "\n")
}

// markdown → 纯文本（无 markdown 权限时的降级路径）在 wen/internal/mdtext.ToPlainText，
// 与微信等其它 IM 通道共用；调用方应先过 sanitizeForQQ（表格与图片的降级两条路径共用）。
