package stylecheck

import (
	"strings"
	"unicode"
)

// Metrics 是一段回复的基础度量。
type Metrics struct {
	Runes     int // 字数：文字与数字的个数，不含标点与空白
	Sentences int // 句数：按句末标点切分
	// BracketRatio 是演绎段内的字数占全部字数的比例（0～1），记号本身不计。
	// 它随时间的变化是「演绎在挤掉话语」或「演绎在消失」的直接信号。
	BracketRatio float64
}

// Measure 计算基础度量。零模型调用、零 I/O。
//
// lang 决定演绎用哪个记号（中文【】、其余语言一对星号，见 acting.go）。不认对
// 记号的话，演绎占比会一直是 0——那正是这个度量最该报警的读数，反而看不出来。
//
// （函数不叫 Metrics：Go 里同一个包内函数与类型不能同名。）
func Measure(lang, text string) Metrics {
	text = strings.TrimSpace(text)
	var m Metrics
	if text == "" {
		return m
	}
	spans := actingSpans(lang, text)
	inActing := 0
	for i, r := range text {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			continue
		}
		m.Runes++
		if inSpans(spans, i) {
			inActing++
		}
	}
	if m.Runes > 0 {
		m.BracketRatio = float64(inActing) / float64(m.Runes)
	}
	m.Sentences = countSentences(spans, text)
	return m
}

// countSentences 数话语与叙述的句数，演绎不算——句数度量的是说了多少话。
// 按句末标点切：中英文的句号、问号、叹号与省略号；连续的标点（「？！」「……」）
// 算一个句末。换行也算一次句末——没有标点的短句各占一行是聊天里的常态。
// 没有任何句末标记但有文字的，算一句。
func countSentences(spans [][]int, text string) int {
	n := 0
	pending := false // 自上一个句末以来是否有过文字
	for i, r := range text {
		if inSpans(spans, i) {
			continue
		}
		switch r {
		case '。', '！', '？', '!', '?', '…', '\n', '；', ';', '.':
			if pending {
				n++
			}
			pending = false
		default:
			if !unicode.IsSpace(r) {
				pending = true
			}
		}
	}
	if pending {
		n++
	}
	return n
}
