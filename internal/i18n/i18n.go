// Package i18n 是界面文案的翻译查表，与 cue、availability 同级：一个谁都能用的
// 小机制，核心不知道被翻译的是「插件」还是别的什么。
//
// 中文不在这里。中文写在产生文案的代码里（插件的 Description、ConfigField 的
// Label 与 Description……），本包只装其余语言，查不到就把中文原样交回去。这样
// 定有三个好处：任何时候都有非空兜底，界面上永远不会露出 plugin.memory.decay
// 这种键；插件作者继续中文优先地写代码，翻译是另一次纯追加的改动，不阻塞功能；
// 读插件代码时看得见文案本身而不是一串键。
//
// 回退是一条链而不是一步：请求日语 → 日语缺 → 英语 → 英语也缺 → 代码里的中文。
// 对一个日语用户来说，英文远比中文近。
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// 语言标识。取值用 BCP 47 的主语言子标签，与浏览器的 navigator.languages
// 和 POSIX 的 LANG 都能对上。
const (
	LangZH = "zh"
	LangEN = "en"
)

// Default 是判定不出语言时的兜底。中文是本项目文案的源语言，缺省用它。
const Default = LangZH

//go:embed locales/*.json
var localeFS embed.FS

// Lang 是一种可选语言。Endonym 是这门语言用它自己的文字写出来的名字——
// 界面上一律用它，不写「Japanese」也不写「日语」：界面已经切成看不懂的语言时，
// 用户唯一还认得出的就是自己文字的那一行，这是这个功能的自救通道。
type Lang struct {
	Code    string
	Endonym string
}

// 支持的语言，顺序即界面上的展示顺序。新增一门语言时在这里加一行，
// 并放一份 locales/<code>.json；别处不需要改。
var langs = []Lang{
	{Code: LangZH, Endonym: "中文"},
	{Code: LangEN, Endonym: "English"},
}

var (
	once  sync.Once
	dicts map[string]map[string]string // 语言 → 键 → 译文；中文不在其中
	load  error
)

func loadOnce() {
	once.Do(func() {
		dicts = make(map[string]map[string]string, len(langs))
		for _, l := range langs {
			if l.Code == LangZH {
				continue // 中文的「字典」就是代码里的原文
			}
			b, err := localeFS.ReadFile("locales/" + l.Code + ".json")
			if err != nil {
				load = fmt.Errorf("读取语言包 %s: %w", l.Code, err)
				return
			}
			var m map[string]string
			if err := json.Unmarshal(b, &m); err != nil {
				load = fmt.Errorf("解析语言包 %s: %w", l.Code, err)
				return
			}
			dicts[l.Code] = m
		}
	})
}

// Languages 返回全部可选语言。
func Languages() []Lang {
	out := make([]Lang, len(langs))
	copy(out, langs)
	return out
}

// Supported 判断某个语言标识是不是本程序支持的。
func Supported(code string) bool {
	for _, l := range langs {
		if l.Code == code {
			return true
		}
	}
	return false
}

// Endonym 返回该语言用自己的文字写出来的名字；未知语言返回其标识本身。
func Endonym(code string) string {
	for _, l := range langs {
		if l.Code == code {
			return l.Endonym
		}
	}
	return code
}

// T 查一条译文，查不到按 lang → 英文 → fallback 的顺序回退。
//
// fallback 是代码里的中文原文，因此**永远不要传空串**：传空就等于允许界面上
// 出现一处空白，而空白比没翻译难查得多。
func T(lang, key, fallback string) string {
	if lang == LangZH || lang == "" || key == "" {
		return fallback
	}
	loadOnce()
	if s, ok := dicts[lang][key]; ok && s != "" {
		return s
	}
	if lang != LangEN {
		if s, ok := dicts[LangEN][key]; ok && s != "" {
			return s
		}
	}
	return fallback
}

// Match 从一串按优先级排好的语言标识里挑出第一个本程序支持的。
//
// 取的是主语言子标签：`zh-Hans-CN`、`zh_CN.UTF-8`、`en-GB` 都能对上。
// 繁体（zh-TW / zh-HK）也归中文——本程序只有简体一份，而繁体读者读简体比读
// 英文近得多；将来真加了 zh-Hant，在 langs 里加一行、这里自然会先匹配到它。
// 一个都对不上时返回空串，由调用方决定用 Default 还是别的。
func Match(prefs ...string) string {
	for _, p := range prefs {
		code := primarySubtag(p)
		if code == "" {
			continue
		}
		// 先试完整的（zh-Hant 这类将来会有的），再退到主子标签
		full := strings.ToLower(strings.ReplaceAll(strings.SplitN(p, ".", 2)[0], "_", "-"))
		if Supported(full) {
			return full
		}
		if Supported(code) {
			return code
		}
	}
	return ""
}

// primarySubtag 从 `zh_CN.UTF-8`、`en-GB`、`ja` 这类写法里取出主语言子标签。
func primarySubtag(tag string) string {
	s := strings.TrimSpace(tag)
	if s == "" || s == "C" || s == "POSIX" {
		return ""
	}
	// 去掉 POSIX 的编码后缀与修饰符：zh_CN.UTF-8@pinyin
	if i := strings.IndexAny(s, ".@"); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, "_", "-")
	if i := strings.Index(s, "-"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// Keys 返回某语言的语言包里已有的全部键，供检查工具比对缺漏。
func Keys(lang string) []string {
	loadOnce()
	out := make([]string, 0, len(dicts[lang]))
	for k := range dicts[lang] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Err 返回语言包的加载错误（如果有）。语言包坏了不该让程序起不来——
// 界面会全部退回中文，但服务照常跑；这个错误留给启动日志与检查工具。
func Err() error {
	loadOnce()
	return load
}

// Tf 是带格式串的那一档：先按语言取出格式串，再套用参数。
//
// 整句进字典而不是在外面拼——中英语序不同，拼出来的句子在另一种语言里必然是错的。
// 需要调换参数顺序时用 %[1]s 这样的显式索引，不要指望译者去改调用点。
func Tf(lang, key, fallback string, a ...any) string {
	return fmt.Sprintf(T(lang, key, fallback), a...)
}
