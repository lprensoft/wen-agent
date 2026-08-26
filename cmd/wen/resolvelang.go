package main

import (
	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/oslocale"
)

// resolveLang 定下这个进程的兜底语言。
//
// 它决定的是**没有 HTTP 请求可依附**的那些路径用哪门语言：发给模型的提示词、
// 插件的状态行、写进会话的注记、运行日志，以及 wen status 这类一次性命令。
// Web UI 与 wen config 仍按请求判定（见 server.reqLang），那条路不经过这里。
//
// 顺序是 配置 > 系统区域 > 中文：
//   - 配置写死的最优先，它是用户明确表达的意愿；写了个不支持的语言按没写算，
//     退到下一档而不是报错——为一个拼错的语言名让服务起不来不值得。
//   - 系统区域是免费的线索，装英文系统的人多半想要英文界面。
//   - 都判不出用中文，它是本项目文案的源语言。
func resolveLang(cfg *config.Config) string {
	if cfg != nil && cfg.Lang != "" && i18n.Supported(cfg.Lang) {
		return cfg.Lang
	}
	if lang := i18n.Match(oslocale.Current()); lang != "" {
		return lang
	}
	return i18n.Default
}

// adoptLang 定下进程语言，并让终端那一侧的输出跟着走。
//
// 每个读配置的命令入口调一次。uiLang 的初值是中文，wen config 的语言菜单在这之后
// 仍能改它（只影响本次进程）——config.yaml 全仓从不回写，要持久化只能去改 lang:，
// 这与模型配置走 models.json 覆盖层是同一条：配置文件里的注释与 ${VAR} 不该被程序
// 改掉。
func adoptLang(cfg *config.Config) string {
	uiLang = resolveLang(cfg)
	return uiLang
}

// adoptLangFromEnv 在还读不到配置时定下语言：帮助文本与未知命令的提示在参数分发
// 之前就要打出来，而那时连 -c 指向哪个配置文件都还不知道。系统区域是此刻唯一的
// 线索；随后每个子命令读到配置会用 adoptLang 覆盖掉它。
func adoptLangFromEnv() {
	uiLang = resolveLang(nil)
}

// langSummary 把语言的判定结果写成一行，供启动日志与 wen status 使用。
//
// 光报一个语言名不够用：判定有两个来源，而「我明明没配过为什么它说英文」正是
// 会来问的那个问题，所以没配的时候要把猜的依据一并说出来。
func langSummary(cfg *config.Config) string {
	name := i18n.Endonym(resolveLang(cfg))
	if cfg != nil && cfg.Lang != "" && i18n.Supported(cfg.Lang) {
		return name
	}
	if raw := oslocale.Current(); raw != "" {
		return trf("lang.fromSystem", "%s（跟随系统区域 %s）", name, raw)
	}
	return trf("lang.fromDefault", "%s（判不出系统区域，用了默认值）", name)
}
