// Package oslocale 报告操作系统的区域设置，形如 zh-CN（Windows）或
// zh_CN.UTF-8（其余平台）。
//
// 从 internal/agent 里抽出来，是因为它有了第二个调用方：环境块要把区域作为一条
// 事实注入模型，而启动时的语言判定要在 Agent 建起来之前就用它猜一门语言
// （见 cmd/wen 的 resolveLang）。两处共用一份，平台差异靠 //go:build 分文件。
//
// 返回的字符串未经归一化，原样交给 i18n.Match 去认——它两种形态都能对上。
package oslocale
