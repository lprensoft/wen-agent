package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"wen/internal/i18n"
	"wen/internal/prompt"
)

// blockToken 认两种块名写法：注入片段用的 [块名]，以及核心那个 <本轮状态>。
// 只认出现在文本最开头或行首的那一处作为「定义」，别处出现的算「引用」。
var blockToken = regexp.MustCompile(`^(\[[^\]\n]{1,60}\]|<[^>\n]{1,60}>)`)

// TestBlockNamesResolveInEveryLanguage 盯住块名的引用图。
//
// 提示词里到处是「以 [对方信息] 与记忆为准」这类互相指认。翻译时两边各译各的，
// 引用方译成 [About Them]、被引用方译成 [About the other person]，就成了一句指向
// 不存在内容的话——模型不会报错，只会照着一个空指代往下演。这类错在中文下不可能
// 发生（两处都是同一个词），只在译文里发生，因此不翻语言包就永远看不见。
//
// 判据锚在中文侧：中文里哪些是块名是确定的（有一处定义它），引用也是确定的。
// 于是要求——中文里引用了块 B 的那句话，它的译文必须引用 B 的译名。
func TestBlockNamesResolveInEveryLanguage(t *testing.T) {
	if err := prompt.Err(); err != nil {
		t.Fatalf("提示词语言包加载失败: %v", err)
	}
	zh := map[string]string{}
	for k, site := range scanRepo(t).Prompt {
		zh[k] = site.ZH
	}

	// 定义：某个键的文本以块名打头，那个键就是这个块的产地。
	// 同一个块名可能有多处定义（如两个插件各注入一次），任一处的译名对上即可。
	owners := map[string][]string{}
	for k, text := range zh {
		if b := leadingBlock(text); b != "" {
			owners[b] = append(owners[b], k)
		}
	}

	for _, l := range i18n.Languages() {
		if l.Code == i18n.LangZH {
			continue // 中文是原文，引用图天然自洽
		}
		var bad []string
		for k, text := range zh {
			lead := leadingBlock(text)
			for _, b := range referencedBlocks(text, lead, owners) {
				want := translationsOf(l.Code, owners[b], zh)
				got := prompt.T(l.Code, k, text)
				if !containsAny(got, want) {
					bad = append(bad, k+" 引用了 "+b+"，译文里却找不到它的译名 "+strings.Join(want, " / "))
				}
			}
		}
		sort.Strings(bad)
		if len(bad) > 0 {
			t.Errorf("语言包 %s 有 %d 处块名引用对不上（模型会照着一个空指代往下演）:\n%s",
				l.Code, len(bad), joinLimit(bad, 20))
		}
	}
}

// leadingBlock 取文本开头的块名；开头不是块名时返回空串。
func leadingBlock(text string) string {
	return blockToken.FindString(strings.TrimSpace(text))
}

// referencedBlocks 找出文本里引用到的、别处定义过的块名（去掉自己开头那一处）。
func referencedBlocks(text, lead string, owners map[string][]string) []string {
	body := strings.TrimSpace(text)
	body = strings.TrimPrefix(body, lead)
	var out []string
	seen := map[string]bool{}
	for b := range owners {
		if b == lead || seen[b] || !strings.Contains(body, b) {
			continue
		}
		seen[b] = true
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// translationsOf 取某个块的全部译名（同一个块名可能有多处定义）。
func translationsOf(lang string, ownerKeys []string, zh map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range ownerKeys {
		if b := leadingBlock(prompt.T(lang, k, zh[k])); b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// suspectRef 找出「看起来像块名、却没有任何地方定义它」的引用。
//
// 这一半盯的是中文原文，与译文无关：agenda 曾让模型「昨天与前几天的事见 [最近经历]」，
// 而那个块叫 [最近几天]，压根没有 [最近经历] 这回事——模型找不到，就自己编一段。
//
// 判据收得很紧，为的是零误报：块名前面必须是空白或行首（排掉「…[已截断]」这类尾注），
// 里面不能有引号（排掉 Schema 里的 JSON 数组）、不能有格式动词（排掉「[超过 %d 字节…]」）、
// 不能是纯数字。剩下的就只有真正的块名写法了。
var suspectRef = regexp.MustCompile(`(?:^|[\s（(])(\[[^\]
"%]{1,40}\])`)

func TestBlockNamesReferencedActuallyExist(t *testing.T) {
	zh := map[string]string{}
	for k, site := range scanRepo(t).Prompt {
		zh[k] = site.ZH
	}
	defs := map[string]bool{}
	for _, text := range zh {
		if b := leadingBlock(text); b != "" {
			defs[b] = true
		}
	}

	var bad []string
	for k, text := range zh {
		body := strings.TrimPrefix(strings.TrimSpace(text), leadingBlock(text))
		seen := map[string]bool{}
		for _, m := range suspectRef.FindAllStringSubmatch(body, -1) {
			tok := m[1]
			if defs[tok] || seen[tok] || isNumeric(tok) {
				continue
			}
			seen[tok] = true
			bad = append(bad, k+" 引用了 "+tok+"，但没有任何地方注入这个块")
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("有 %d 处引用指向不存在的块（模型找不到，就会自己编一段）:\n%s",
			len(bad), joinLimit(bad, 20))
	}
}

func isNumeric(tok string) bool {
	inner := strings.Trim(tok, "[]")
	if inner == "" {
		return false
	}
	for _, r := range inner {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return len(subs) == 0 // 被引用的块自己没有译文时不追究，那由覆盖测试报
}
