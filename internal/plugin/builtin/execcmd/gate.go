package execcmd

import (
	"context"
	"fmt"

	"wen/internal/i18n"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// gate 在执行前按风险分级放行、拒绝或请求用户确认。
// 返回非 nil 的 error 表示这条命令不执行——错误文本会作为工具结果回给模型，
// 因此要说清是被拒绝还是未获同意，以及不要绕过。
func (t *tool) gate(ctx context.Context, s settings, command string) error {
	v, reason := classify(command, s.guard, s.lang)
	switch v {
	case verdictAllow:
		return nil
	case verdictDeny:
		i18n.Logf(s.lang, "log.execcmd.blocked", "exec_command 拒绝执行（%s）: %s", reason, command)
		return fmt.Errorf(prompt.T(s.lang, "prompt.exec_command.result.denied",
			"这条命令被拒绝执行：%s。这类操作不可撤销且影响整台机器，请改用范围更小的做法，不要尝试绕过"), reason)
	}

	confirm, ok := plugin.ConfirmerFrom(ctx)
	if !ok {
		// 没有可交互的界面时一律拒绝，不给「无人值守就放行」的开关：
		// 拿不到答复不等于得到许可。确实要在无人值守下跑破坏性命令的，
		// 把 guard 关掉——那是一个明确的、看得见的选择，而不是一个容易被忘记的例外。
		i18n.Logf(s.lang, "log.execcmd.unattended", "exec_command 无人值守拒绝（%s）: %s", reason, command)
		return fmt.Errorf(prompt.T(s.lang, "prompt.exec_command.result.no_ui",
			"这条命令需要对方确认（%s），但当前没有可交互的界面，已按拒绝处理。"+
				"请改用不需要确认的做法，或告知对方手动执行"), reason)
	}

	cctx := ctx
	if s.confirmTimeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, s.confirmTimeout)
		defer cancel()
	}
	approved, err := confirm(cctx, plugin.ConfirmRequest{
		Source: "exec_command",
		Title:  prompt.T(s.lang, "prompt.exec_command.confirm.title", "执行命令"),
		Detail: truncateRunes(s.lang, command, confirmDetailMaxRunes),
		Reason: reason,
	})
	if err != nil {
		i18n.Logf(s.lang, "log.execcmd.no_answer", "exec_command 确认未完成（%v）: %s", err, command)
		return fmt.Errorf(prompt.T(s.lang, "prompt.exec_command.result.no_answer",
			"这条命令需要对方确认（%s），但没有得到答复（%v），已按拒绝处理"), reason, err)
	}
	if !approved {
		i18n.Logf(s.lang, "log.execcmd.refused", "exec_command 用户拒绝（%s）: %s", reason, command)
		return fmt.Errorf(prompt.T(s.lang, "prompt.exec_command.result.refused",
			"对方拒绝执行这条命令（%s）。请说明你打算做什么并另找办法，不要改写命令重试"), reason)
	}
	return nil
}

// truncateRunes 按字符数截断，不切断多字节字符。
//
// 它的产物进确认弹窗，只给人看；跟随进程语言，与同一次拦截里那句 reason 一致。
func truncateRunes(lang, s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + prompt.Tf(lang, "prompt.exec_command.confirm.truncated",
		"…（共 %d 字，已截断）", len(r))
}
