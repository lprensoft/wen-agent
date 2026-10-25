package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"wen/internal/plugin"
)

// 插件操作的终端跟踪：触发后轮询进展，文字逐条打印，带链接时渲染成字符二维码
// ——微信的扫码绑定在远程 SSH 机器上就靠这条路，不必为扫个码打开 Web UI。

const (
	actionPollInterval = time.Second
	// actionWaitMax 是终端侧等待的上限，略长于最长的绑定流程（8 分钟）。
	// 到时只是不再看，服务端流程照常进行——与 Web UI 关掉进展窗同一个语义。
	actionWaitMax = 10 * time.Minute
)

// runAction 触发一个插件操作并跟踪到结束。
func runAction(b backend, pluginName string, act plugin.ActionDef) error {
	if err := b.startPluginAction(pluginName, act.Key, act.Token); err != nil {
		return err
	}
	note(trf("cli.action.started", "▶ %s 已开始（Ctrl+C 只是退出查看，不会中断服务端流程）", act.Label))

	lastMsg, lastLink := "", ""
	deadline := time.Now().Add(actionWaitMax)
	for time.Now().Before(deadline) {
		st, err := b.pluginActionState(pluginName, act.Key)
		if err != nil {
			return err
		}
		switch st.Status {
		case plugin.ActionDone:
			if st.Message != "" && st.Message != lastMsg {
				note("%s", st.Message)
			}
			return nil
		case plugin.ActionError:
			if st.Message != "" {
				return errors.New(st.Message)
			}
			return errors.New(tr("cli.action.failed", "操作失败"))
		}
		if st.Message != "" && st.Message != lastMsg {
			note("%s", st.Message)
			lastMsg = st.Message
		}
		if st.Link != "" && st.Link != lastLink {
			if qr, err := terminalQR(st.Link); err == nil {
				fmt.Print(qr)
			}
			note(tr("cli.action.qrFallback", "扫不出时可复制链接自行生成二维码："))
			note("%s", st.Link)
			lastLink = st.Link
		}
		time.Sleep(actionPollInterval)
	}
	return errors.New(tr("cli.action.timeout", "等待超时，流程可能仍在服务端进行，可稍后重新进入查看"))
}

// terminalQR 把链接渲染成终端字符二维码（半块字符，两行并一行）。
//
// 黑白用 ANSI 颜色显式指定而不跟随终端配色：扫码器认的是「深码浅底」，
// 深色主题的终端若按默认前景/背景画，颜色正好反过来，扫不出来。
func terminalQR(content string) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bm := q.Bitmap() // true = 深色模块；四周自带静区
	var b strings.Builder
	for y := 0; y < len(bm); y += 2 {
		for x := range bm[y] {
			bottom := y+1 < len(bm) && bm[y+1][x]
			b.WriteString(halfBlock(bm[y][x], bottom))
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String(), nil
}

// halfBlock 用 ▀ 画一格：前景色是上半格，背景色是下半格。
func halfBlock(top, bottom bool) string {
	fg, bg := "97", "107" // 亮白
	if top {
		fg = "30" // 黑
	}
	if bottom {
		bg = "40"
	}
	return "\x1b[" + fg + ";" + bg + "m▀"
}
