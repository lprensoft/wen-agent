package agent

import (
	"fmt"
	"runtime"

	"wen/internal/oslocale"
)

// envContext 生成注入 system 消息开头的系统环境块，
// 让模型无需在每个工具描述里重复平台信息。内容为纯功能性描述。
//
// 这里只放**整轮之间不变**的事实。当前时间不在其中：它每轮都变，放在这个位置会
// 一并作废掉整段 system 与历史的提示词缓存前缀，而且离生成位置最远、最容易被历史
// 里出现过的旧时刻盖过去。它由 turnStateBlock 放到历史之后（见 turnstate.go）。
func envContext(workdir string) string {
	shell := "sh -c"
	if runtime.GOOS == "windows" {
		shell = "cmd /C（请使用 Windows 命令语法，如 dir、type、findstr）"
	}
	locale := oslocale.Current()
	if locale == "" {
		locale = "未知"
	}
	return fmt.Sprintf(
		"[系统环境]\n操作系统: %s/%s\nShell: %s\n工作目录: %s\n系统区域与语言: %s",
		runtime.GOOS, runtime.GOARCH, shell, workdir, locale,
	)
}
