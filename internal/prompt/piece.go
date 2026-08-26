package prompt

// Piece 是一段**声明在别处、用在别处**的提示词文本。
//
// 大部分提示词就写在用它的那一行上，T 就够了。但有些是先摆成一张表再按情况取用的：
// execcmd 的十七类风险名、各插件的中文枚举值都是这个形状。那些地方 T 的调用点上
// 只有一个变量，键与原文都在表里——而覆盖检查是靠静态扫源码找 (键, 原文) 对的
// （见 cmd/wen 的 scanRepo），扫不到就等于那批文本永远不会被提醒漏译。
//
// Text 把这一对绑成一个值，声明处仍是两个字面量，扫描因此看得见；取用处只剩
// 一次 For(lang)。
type Piece struct {
	key string
	zh  string
}

// Text 声明一段提示词：键与中文原文。两个都要是字面量，否则覆盖检查扫不到。
func Text(key, zh string) Piece { return Piece{key: key, zh: zh} }

// Key 返回这段文本的键。
func (p Piece) Key() string { return p.key }

// ZH 返回中文原文。它同时是查不到译文时的兜底，因此不会是空串。
func (p Piece) ZH() string { return p.zh }

// For 按语言取出这段文本。
func (p Piece) For(lang string) string { return T(lang, p.key, p.zh) }

// Format 是带格式串的那一档。
func (p Piece) Format(lang string, a ...any) string { return Tf(lang, p.key, p.zh, a...) }
