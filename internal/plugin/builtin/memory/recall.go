package memory

import (
	"math"
	"slices"
	"strings"
	"unicode"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 本文件是「按相关性自动召回」：用本轮的用户输入对可读的记忆打分，把最相关的几条
// **正文**随 <本轮状态> 注入。索引那条路只是目录——活人是聊到这件事自然想起来，
// 不是先查目录再决定翻不翻，而模型实测很少主动 recall_memory。
//
// 打分是纯词面匹配，零模型调用、零依赖：连续的中日韩文字按相邻两字切成二元组，
// 其余文字按单词（小写）切，去掉一小份虚词表；命中标题或摘要的权重是命中正文的三倍，
// 再按条目长度做对数抑制。它不懂同义词，但一轮对话里提到的名字、地点、物件多半
// 会原样出现在记忆的标题或正文里，这就够用了。

const (
	defaultRecallMax        = 2
	defaultRecallEntryRunes = 400
	defaultRecallMaxBytes   = 2048
	defaultRecallMinScore   = 4

	// titleHitWeight / contentHitWeight 是一次命中的分值：标题与摘要是保存时写下的
	// 「这条讲什么」，命中它比正文里碰巧出现同一个词可信得多。
	titleHitWeight   = 3
	contentHitWeight = 1
	// contentOnlyHits 是标题与摘要都没命中时，单凭正文放行所需的命中数。
	contentOnlyHits = 4
	// minInputTokens 以下的输入不召回：「嗯」「好的」不该触发翻记忆。
	minInputTokens = 4
	// minRecallBodyBytes 是一条正文在预算里至少要占到的字节数，再少就只剩半句话，
	// 不如不放。
	minRecallBodyBytes = 120
	// recallEllipsisZH 附在被截断的正文末尾，让模型知道后面还有、可按标题读全文。
	recallEllipsisZH = "……（后文略，可用 recall_memory 读全文）"
)

// recallHeaderZH 领起召回块。块本身不解释用法——那一句在 promptGuideZH 里，每轮稳定不变。
const recallHeaderZH = "[想起来的事]"

// stopTokens 是不参与打分的虚词：它们在任何一段话里都会出现，命中只是噪音。
// 中文按二元组列，英文按单词列。
var stopTokens = map[string]bool{
	"我们": true, "你们": true, "他们": true, "她们": true, "它们": true, "咱们": true,
	"的话": true, "什么": true, "这个": true, "那个": true, "这些": true, "那些": true,
	"可以": true, "一下": true, "知道": true, "时候": true, "没有": true, "就是": true,
	"不是": true, "怎么": true, "这样": true, "那样": true, "这么": true, "那么": true,
	"因为": true, "所以": true, "但是": true, "然后": true, "如果": true, "还是": true,
	"或者": true, "已经": true, "现在": true, "觉得": true, "应该": true, "可能": true,
	"一个": true, "一些": true, "有点": true, "不过": true, "其实": true, "真的": true,
	"还有": true, "而且": true, "只是": true, "虽然": true, "这里": true, "那里": true,
	"自己": true, "今天": true, "明天": true, "昨天": true, "一样": true, "一直": true,
	"好像": true, "感觉": true, "东西": true, "事情": true, "问题": true, "需要": true,
	"是不": true, "不会": true, "不要": true, "不用": true, "不能": true, "不知": true,
	"怎样": true, "为什": true,
	"the": true, "and": true, "for": true, "are": true, "but": true, "not": true,
	"you": true, "with": true, "this": true, "that": true, "have": true, "was": true,
	"what": true, "your": true, "can": true, "just": true, "about": true, "from": true,
}

// tokenize 把一段文本切成打分用的 token 集合（去重、去虚词）。
//
// 连续的中日韩文字按相邻两字切成二元组（「养猫」「猫叫」），其余字母数字按单词切并
// 小写。单个中日韩字符不成 token：一个字太含糊，命中它说明不了什么。单字母的英文
// 单词同理。
func tokenize(text string) map[string]bool {
	out := map[string]bool{}
	var cjk []rune
	var word []rune
	flushCJK := func() {
		for i := 0; i+1 < len(cjk); i++ {
			addToken(out, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	flushWord := func() {
		if len(word) >= 2 {
			addToken(out, strings.ToLower(string(word)))
		}
		word = word[:0]
	}
	for _, r := range text {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushCJK()
			flushWord()
		}
	}
	flushCJK()
	flushWord()
	return out
}

func addToken(set map[string]bool, tok string) {
	if !stopTokens[tok] {
		set[tok] = true
	}
}

// isCJK 判断一个字符是否按「相邻两字成词」的方式切分。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// recallHit 是一条记忆对本轮输入的打分结果。
type recallHit struct {
	entry Entry
	// titleHits / contentHits 分别是命中标题+摘要与命中正文的 token 数。
	titleHits   int
	contentHits int
	// raw 是加权命中数，阈值看它：标题命中一个算 titleHitWeight 分，正文命中一个算
	// contentHitWeight 分——这样配置里的「最低分」才说得清是几次命中。
	raw int
	// score 是 raw 按条目长度做对数抑制后的排序分。只用来排序：同样命中三个词，
	// 二十个词的短条目比三百个词的长条目可信得多。
	score float64
}

// scoreEntry 给一条记忆打分。query 是已切好的输入 token 集合。
func scoreEntry(query map[string]bool, e Entry) recallHit {
	title := tokenize(e.Name + " " + e.Description)
	content := tokenize(e.Content)
	h := recallHit{entry: e}
	for tok := range query {
		if title[tok] {
			h.titleHits++
		}
		if content[tok] {
			h.contentHits++
		}
	}
	h.raw = h.titleHits*titleHitWeight + h.contentHits*contentHitWeight
	total := len(title)
	for tok := range content {
		if !title[tok] {
			total++
		}
	}
	h.score = float64(h.raw) / math.Log(float64(total)+math.E)
	return h
}

// passes 判断这条命中够不够格被召回：标题或摘要至少命中一个且加权分达到 minScore，
// 或者正文独自命中得足够多。
func (h recallHit) passes(minScore int) bool {
	if h.titleHits > 0 && h.raw >= minScore {
		return true
	}
	return h.contentHits >= contentOnlyHits
}

// pickRecalls 从候选里挑出最多 max 条值得召回的记忆，按相关性排序；同分的更新时间
// 新的在前。输入太短（不足 minInputTokens 个 token）时不召回。
func pickRecalls(input string, entries []Entry, max, minScore int) []recallHit {
	if max <= 0 {
		return nil
	}
	query := tokenize(input)
	if len(query) < minInputTokens {
		return nil
	}
	var hits []recallHit
	for _, e := range entries {
		h := scoreEntry(query, e)
		if h.passes(minScore) {
			hits = append(hits, h)
		}
	}
	slices.SortStableFunc(hits, func(a, b recallHit) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return b.entry.Updated.Compare(a.entry.Updated)
	})
	if len(hits) > max {
		hits = hits[:max]
	}
	return hits
}

// renderRecalls 把召回的记忆渲染成注入块，正文按 entryRunes 截断，整块不超过 maxBytes。
// 装不下时按排序靠后的先让：一条正文能塞进的部分少于 minRecallBodyBytes 就不再放，
// 半句话没有信息量。返回块文本与实际放进去的条数；一条都装不下时块为空串。
func renderRecalls(lang string, hits []recallHit, entryRunes, maxBytes int) (string, int) {
	var b strings.Builder
	b.WriteString(recallHeaderFor(lang))
	ellipsis := recallEllipsisFor(lang)
	n := 0
	for _, h := range hits {
		e := h.entry
		head := "\n\n" + prompt.Tf(lang, "prompt.memory.recall.entry_head", "「%s/%s」（更新于 %s）",
			memTypes.For(lang, e.Type), e.Name, e.Updated.Format("2006-01-02")) + "\n"
		body := strings.TrimSpace(e.Content)
		if entryRunes > 0 && len([]rune(body)) > entryRunes {
			body = truncateRunes(body, entryRunes) + ellipsis
		}
		if maxBytes > 0 {
			room := maxBytes - b.Len() - len(head)
			if room < minRecallBodyBytes {
				break
			}
			if len(body) > room {
				cut, _ := plugin.TruncateUTF8([]byte(body), room-len(ellipsis))
				body = strings.TrimSpace(string(cut)) + ellipsis
			}
		}
		b.WriteString(head)
		b.WriteString(body)
		n++
	}
	if n == 0 {
		return "", 0
	}
	return b.String(), n
}
