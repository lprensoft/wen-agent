// Package prompt 是**给模型看的**文本的按语言取用：注入片段、块名、工具描述与
// 参数 Schema、工具返回文本、面向模型的错误。
//
// 与 internal/i18n 并列而不合并，因为两者的判据不同：界面文案缺译只是界面丑，
// 而提示词缺译等于模型收到一份半中半英的指令——同一个块名在一处是中文、另一处
// 是英文时，模型不会报错，只会照着一个空指代往下演。语言包因此分开放，覆盖测试
// 也各盯各的一份。
//
// 中文不在语言包里。它留在产生它的 Go 代码里，既是源语言也是查不到时的兜底，
// 与 i18n 的取舍一致：任何时候都有非空兜底，插件作者继续中文优先地写，翻译是
// 另一次纯追加的改动；读插件代码时看得见提示词本身，而不是一串键。
//
// 回退是一条链：请求日语 → 日语缺 → 英语 → 英语也缺 → 代码里的中文。
package prompt

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"wen/internal/i18n"
)

//go:embed locales/*.json
var localeFS embed.FS

var (
	once  sync.Once
	dicts map[string]map[string]string // 语言 → 键 → 译文；中文不在其中
	load  error
)

func loadOnce() {
	once.Do(func() {
		dicts = make(map[string]map[string]string)
		for _, l := range i18n.Languages() {
			if l.Code == i18n.LangZH {
				continue // 中文的「字典」就是代码里的原文
			}
			b, err := localeFS.ReadFile("locales/" + l.Code + ".json")
			if err != nil {
				load = fmt.Errorf("读取提示词语言包 %s: %w", l.Code, err)
				return
			}
			var m map[string]string
			if err := json.Unmarshal(b, &m); err != nil {
				load = fmt.Errorf("解析提示词语言包 %s: %w", l.Code, err)
				return
			}
			dicts[l.Code] = m
		}
	})
}

// Err 报告语言包的加载错误。一份坏掉的语言包不该让服务起不来——它只是让那门
// 语言退回中文，所以错误在这里等着被问，而不是在 init 里 panic。
func Err() error {
	loadOnce()
	return load
}

// T 查一段提示词，查不到按 lang → 英文 → zh 的顺序回退。
//
// zh 是代码里的中文原文，**永远不要传空串**：空串会让模型收到一段什么都没说的
// 指令，比没翻译难查得多。
//
// lang 为空（插件尚未 Init、Manager 对禁用插件调 SystemPrompt 的那条路径）
// 直接回中文，不查表也不 panic。
func T(lang, key, zh string) string {
	if lang == i18n.LangZH || lang == "" || key == "" {
		return zh
	}
	loadOnce()
	if s, ok := dicts[lang][key]; ok && s != "" {
		return s
	}
	if lang != i18n.LangEN {
		if s, ok := dicts[i18n.LangEN][key]; ok && s != "" {
			return s
		}
	}
	return zh
}

// Tf 是带格式串的那一档：先按语言取出格式串，再套用参数。
//
// 参数的顺序在两种语言里必须一致——需要调换顺序时用 %[1]s 这样的显式索引，
// 不要指望译者去改调用点。
func Tf(lang, key, zh string, a ...any) string {
	return fmt.Sprintf(T(lang, key, zh), a...)
}

// Keys 返回某门语言的语言包里全部的键，已排好序。供覆盖测试查死键用。
func Keys(lang string) []string {
	loadOnce()
	out := make([]string, 0, len(dicts[lang]))
	for k := range dicts[lang] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
