package agent

import (
	"runtime"

	"wen/internal/oslocale"
	"wen/internal/prompt"
)

const envZH = "[系统环境]\n操作系统: %s/%s\nShell: %s\n工作目录: %s\n系统区域与语言: %s"

const envShellWindowsZH = "cmd /C（请使用 Windows 命令语法，如 dir、type、findstr）"

const envLocaleUnknownZH = "未知"

// envContext 生成注入 system 消息开头的系统环境块，
// 让模型无需在每个工具描述里重复平台信息。内容为纯功能性描述。
//
// 这里只放**整轮之间不变**的事实。当前时间不在其中：它每轮都变，放在这个位置会
// 一并作废掉整段 system 与历史的提示词缓存前缀，而且离生成位置最远、最容易被历史
// 里出现过的旧时刻盖过去。它由 turnStateBlock 放到历史之后（见 turnstate.go）。
//
// 块名随语言整体切换。引用它的提示词（roleplay 的 [时间一致性] 会说「[系统环境]
// 中的其余项……不构成角色的自我认知」）必须同语言引用同一个名字——语言是进程级的，
// 两边取的是同一个 lang，因此只要两处的译文对得上就不会指空。
func envContext(lang, workdir string) string {
	shell := "sh -c"
	if runtime.GOOS == "windows" {
		shell = prompt.T(lang, "prompt.core.env.shell_windows", envShellWindowsZH)
	}
	locale := oslocale.Current()
	if locale == "" {
		locale = prompt.T(lang, "prompt.core.env.locale_unknown", envLocaleUnknownZH)
	}
	return prompt.Tf(lang, "prompt.core.env", envZH,
		runtime.GOOS, runtime.GOARCH, shell, workdir, locale)
}
