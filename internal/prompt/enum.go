package prompt

import (
	"encoding/json"

	"wen/internal/i18n"
)

// Enum 是一组会被**落盘**的枚举值：工具 Schema 里给模型看的候选项，模型选中之后
// 原样存进插件自己的文件。
//
// 它与 Piece 的区别只在这一点上，而这一点决定了做法：一段普通提示词换了语言，下次
// 注入的是新语言那份，没有后果；一个枚举值换了语言，磁盘上已经躺着一批中文的旧值，
// 而新写进去的是英文的——同一个字段从此有两种写法，比对、分档、统计全部对不上。
//
// 因此**中文值是规范形式**：Values 按语言给出 Schema 里的候选项，Canonical 把模型
// 报回来的任意语言的字面量换回中文再落盘。已经存下的数据一个字都不用动。
//
// 声明处仍是一串 Text(键, 中文)，覆盖检查照常扫得到。
type Enum struct{ items []Piece }

// NewEnum 按候选项的展示顺序声明一组枚举值。
func NewEnum(items ...Piece) Enum { return Enum{items: items} }

// Values 按语言给出候选项，顺序与声明一致。
func (e Enum) Values(lang string) []string {
	out := make([]string, 0, len(e.items))
	for _, it := range e.items {
		out = append(out, it.For(lang))
	}
	return out
}

// ZHValues 给出全部中文规范值，顺序与声明一致。落盘的比对、排序与「这个值合不合法」
// 都按它来——那一档只认规范形式，与展示无关，因此不经语言。
func (e Enum) ZHValues() []string {
	out := make([]string, 0, len(e.items))
	for _, it := range e.items {
		out = append(out, it.ZH())
	}
	return out
}

// JSON 给出可直接拼进 Schema 的 JSON 数组。手拼字符串迟早会拼出一个带引号的值。
func (e Enum) JSON(lang string) string {
	b, err := json.Marshal(e.Values(lang))
	if err != nil || len(b) == 0 {
		return "[]"
	}
	return string(b)
}

// Canonical 把任意语言的字面量换回中文规范值。
//
// 认所有语言而不只是当前那门：模型偶尔会照着历史里见过的旧值回答，而历史可能是
// 换语言之前写下的。第二个返回值为 false 表示这个值不在表里，调用方按参数非法处理
// ——不要「猜一个最接近的」，那会把一次明确的错误变成一条悄悄存错的记录。
func (e Enum) Canonical(v string) (string, bool) {
	for _, it := range e.items {
		if v == it.ZH() {
			return it.ZH(), true
		}
		for _, l := range i18n.Languages() {
			if l.Code != i18n.LangZH && v == it.For(l.Code) {
				return it.ZH(), true
			}
		}
	}
	return "", false
}

// Has 判断一个中文规范值是否属于本组。
func (e Enum) Has(zh string) bool {
	for _, it := range e.items {
		if it.ZH() == zh {
			return true
		}
	}
	return false
}

// For 把一个中文规范值按语言译回去，供回执与注入使用。不在表里的原样返回
// ——那是手改过的文件里的值，照原样说出来比换成一个像模像样的错值好。
func (e Enum) For(lang, zh string) string {
	for _, it := range e.items {
		if it.ZH() == zh {
			return it.For(lang)
		}
	}
	return zh
}
