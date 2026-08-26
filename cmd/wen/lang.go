package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/charmbracelet/huh"

	"wen/internal/i18n"
)

// 终端配置界面的语言。
//
// **刻意不持久化**：每次进来都是中文，选过的语言随进程结束就忘。wen config 是
// 几个月才进一次的东西，人就坐在终端前，多按两下方向键比多一个状态文件划算——
// 而多出来的那个文件会带来「它和 Web UI 那份谁说了算」这个必须记住的规则，
// 那正是插件配置曾经踩过的坑。Web UI 那边相反：浏览器每天开、语言由浏览器免费
// 报给我们、存一行 localStorage 的成本约等于零，所以那边是三态加持久化。
//
// 用包级变量而不是往每个函数里传：配置界面是单线程的一问一答，一次只有一个人
// 在用；把它穿进二十几个渲染函数的签名只会让每一处都多一个参数。
var uiLang = i18n.Default

// tr 查一条界面文案。zh 是代码里的中文原文，同时充当查不到时的兜底，
// 因此**不要传空串**——传空就等于允许界面上出现一处空白。
func tr(key, zh string) string {
	return i18n.T(uiLang, key, zh)
}

// trf 是带占位符的版本。整句进字典，不要在外面拼——中英语序不同，
// 拼出来的句子在另一种语言里必然是错的。
func trf(key, zh string, a ...any) string {
	return fmt.Sprintf(i18n.T(uiLang, key, zh), a...)
}

// logf 打一行运行日志（措辞同样按语言取，见 i18n.Logf 的说明）。与 trf 同形，
// 只是去处从标准输出换成日志。
func logf(key, zh string, a ...any) {
	i18n.Logf(uiLang, key, zh, a...)
}

// fatalf 打一行日志后退出。启动阶段那几处「起不来就别起」用它。
func fatalf(key, zh string, a ...any) {
	log.Fatal(trf(key, zh, a...))
}

// languageMenu 让用户挑一种界面语言。
//
// 每种语言都用它自己的文字写（中文 / English / 日本語……），不写「Japanese」也不
// 写「英语」：界面已经切成看不懂的语言时，用户唯一还认得出的就是自己文字的那
// 一行。这是这个功能的自救通道，也是它必须排在顶层菜单第一项的理由。
func languageMenu() error {
	choice := uiLang
	opts := make([]huh.Option[string], 0, len(i18n.Languages()))
	for _, l := range i18n.Languages() {
		opts = append(opts, huh.NewOption(l.Endonym, l.Code))
	}
	err := run(huh.NewSelect[string]().
		Title("Language").
		Description(tr("cli.language.desc", "选定后返回上级，菜单与插件说明一并切换。")).
		Options(opts...).
		Height(listHeight(len(opts), 3)).
		Value(&choice))
	if err != nil {
		return err
	}
	uiLang = choice
	return nil
}

// languageEntry 是顶层菜单里那一项的标签：`Language: 中文`。
//
// 「Language」这个词不翻译。它是所有语言的使用者都认得的那个英文单词，而这一项
// 存在的意义正是「我看不懂这个界面时还能找到它」——翻成当前语言就把这条路堵死了。
func languageEntry() string {
	return "Language: " + i18n.Endonym(uiLang)
}

// joinList 按当前语言的习惯连接一串词。中文用顿号、西文用逗号加空格——
// 这种标点差异逐处硬写会漏，集中在这里。
func joinList(ss []string) string {
	return strings.Join(ss, tr("cli.list.sep", "、"))
}

// groupLabel 是插件列表里那个分组标记。中文用【】，西文里那对括号又高又怪，
// 换成方括号——所以连括号一起进字典，而不是在外面拼。
func groupLabel(cat string) string {
	return trf("cli.plugins.group", "【%s】", cat)
}
