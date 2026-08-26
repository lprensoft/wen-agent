package main

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"wen/internal/i18n"
	"wen/internal/prompt"
)

// verbPattern 认出一个格式动词：可选的显式索引、可选的标志与宽度，最后那个字母。
var verbPattern = regexp.MustCompile(`%(\[\d+\])?[-+# 0]*[0-9*]*(\.[0-9*]+)?[a-zA-Z]`)

// formatVerbs 取出一段格式串里的动词，换算成「它要一个什么样的实参」再排序。
// %% 是转义的百分号，不算。
//
// 比的是实参的类型而不是动词本身：中文用「」括起一个值，英文那边换成 %q 让 fmt
// 自己加引号，这是对的译法，不是错配。真正会出事的是个数对不上，或者拿 %d 去接
// 一个字符串——那才是 %!d(string=…) 的来历。
func formatVerbs(s string) []string {
	var out []string
	for _, m := range verbPattern.FindAllString(strings.ReplaceAll(s, "%%", ""), -1) {
		out = append(out, verbClass(m[len(m)-1]))
	}
	sort.Strings(out)
	return out
}

// verbClass 把动词归到实参类型上。%v 归「任意」，它对什么都成立。
func verbClass(v byte) string {
	switch v {
	case 'v':
		return "任意"
	case 's', 'q':
		return "字符串"
	case 'd', 'b', 'o', 'O', 'x', 'X', 'c', 'U', 'f', 'e', 'E', 'g', 'G':
		return "数值"
	case 't':
		return "布尔"
	}
	return string(v)
}

// verbsCompatible 判断两串动词能不能对上：个数必须一样，逐类配对，配不上的交给
// 另一侧的「任意」去接。
func verbsCompatible(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left, right := append([]string(nil), a...), append([]string(nil), b...)
	for i := 0; i < len(left); {
		if j := indexOf(right, left[i]); j >= 0 {
			left = append(left[:i], left[i+1:]...)
			right = append(right[:j], right[j+1:]...)
			continue
		}
		i++
	}
	// 剩下的必须一一由某一侧的「任意」接住
	for _, v := range left {
		if v == "任意" {
			continue
		}
		j := indexOf(right, "任意")
		if j < 0 {
			return false
		}
		right = append(right[:j], right[j+1:]...)
	}
	return true
}

func indexOf(ss []string, v string) int {
	for i, s := range ss {
		if s == v {
			return i
		}
	}
	return -1
}

// TestFormatVerbsMatchAcrossLanguages 盯住译文的占位符。
//
// 中英语序不同，因此译文常要用 %[2]s 这样的显式索引调换顺序——而调换的时候很容易
// 多一个、少一个，或者把 %d 写成 %s。结果不是报错，是日志里出现一句
// 「%!d(MISSING)」，只有读英文日志的人会看见，而那正是最不会有人去读的地方。
//
// 判据是**动词的多重集**而不是位置：顺序本来就允许变，个数与类型不允许。
func TestFormatVerbsMatchAcrossLanguages(t *testing.T) {
	scan := scanRepo(t)
	check := func(pack, key, zh string, translated func(lang string) string) {
		want := formatVerbs(zh)
		for _, l := range i18n.Languages() {
			if l.Code == i18n.LangZH {
				continue
			}
			got := formatVerbs(translated(l.Code))
			if !verbsCompatible(want, got) {
				t.Errorf("%s 包 %s 的 %s 译文占位符对不上：中文 %v，译文 %v\n中文: %s\n译文: %s",
					pack, key, l.Code, want, got, zh, translated(l.Code))
			}
		}
	}
	for k, site := range scan.UI {
		check("i18n", k, site.ZH, func(lang string) string { return i18n.T(lang, k, site.ZH) })
	}
	for k, site := range scan.Prompt {
		check("prompt", k, site.ZH, func(lang string) string { return prompt.T(lang, k, site.ZH) })
	}
}

// 这把尺子同样要能证明自己会响：判据一旦收得太松，上面那条就永远绿着。
func TestVerbsCompatible(t *testing.T) {
	for _, c := range []struct {
		zh, en string
		ok     bool
	}{
		{"记了 %d 条（%s）", "%d entries (%s)", true},
		{"叫「%s」的", "called %q", true},            // 中文用「」，英文交给 %q
		{"%s 在 %s", "%[2]s at %[1]s", true},      // 只调换顺序
		{"记了 %d 条（%s）", "%d entries", false},     // 少一个
		{"记了 %d 条", "%d entries (%s)", false},    // 多一个
		{"会话 %s 忙", "session %d is busy", false}, // 字符串接进了 %d
		{"耗时 %v", "took %s", true},               // %v 什么都接得住
		{"完成度 %d%%", "%d%% done", true},          // %% 不是动词
	} {
		if got := verbsCompatible(formatVerbs(c.zh), formatVerbs(c.en)); got != c.ok {
			t.Errorf("verbsCompatible(%q, %q) = %v，期望 %v", c.zh, c.en, got, c.ok)
		}
	}
}
