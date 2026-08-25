package wechatbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"wen/internal/plugin"
)

// credentials 是扫码绑定后服务端下发的凭证，持久化到 StateDir/credentials.json（0600）。
type credentials struct {
	BotToken     string `json:"bot_token"`
	BaseURL      string `json:"base_url"` // 该 bot 专属的 API 基址，绑定后的请求都用它
	BotID        string `json:"bot_id"`
	BinderUserID string `json:"binder_user_id"` // 扫码绑定人，始终在白名单内
}

func (c credentials) valid() bool { return c.BotToken != "" && c.BaseURL != "" }

func credsPath(stateDir string) string { return filepath.Join(stateDir, "credentials.json") }

func loadCredentials(stateDir string) (credentials, error) {
	var c credentials
	raw, err := os.ReadFile(credsPath(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, nil
}

func saveCredentials(stateDir string, c credentials) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	tmp := credsPath(stateDir) + ".tmp"
	// 0600：bot_token 等同于登录态
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, credsPath(stateDir))
}

// ---------- 扫码绑定（插件操作入口） ----------

// bindState 是绑定流程的当前状态，由界面轮询展示。cancel 非 nil 表示流程进行中；
// gen 是流程代次——重复触发会取消旧流程并 +1，旧 goroutine 凭代次识别自己已被替换。
type bindState struct {
	status  string
	message string
	png     []byte
	link    string // 编码进二维码的 URL 原文，终端界面据此自渲染
	cancel  context.CancelFunc
	gen     int
}

const (
	bindTimeout    = 8 * time.Minute // 整个绑定流程的上限
	qrPollInterval = time.Second
	maxQRRefresh   = 3 // 二维码过期自动刷新次数
)

func (p *Plugin) Actions() []plugin.ActionDef {
	p.mu.Lock()
	bound := p.creds.valid()
	p.mu.Unlock()
	// 标签固定、变化的部分放 Detail：「已绑定」是运行时事实，掺进说明里那句话就进不了
	// 语言包了（见 ActionDef.Detail）。
	detail := ""
	if bound {
		detail = "已绑定。重新扫码会替换当前凭证。"
	}
	return []plugin.ActionDef{{
		Key:         "bind",
		Label:       "扫码绑定微信",
		Description: "获取动态二维码，用微信扫码确认后完成绑定（微信 App：我 → 设置 → 插件 → ClawBot）",
		Detail:      detail,
	}}
}

// StartAction 启动绑定流程。进行中重复触发 = 取消旧流程重新开始（二维码可能已过期）。
func (p *Plugin) StartAction(_ context.Context, key string) error {
	if key != "bind" {
		return fmt.Errorf("没有操作 %q", key)
	}
	p.mu.Lock()
	if p.ctx == nil {
		p.mu.Unlock()
		return fmt.Errorf("插件尚未初始化")
	}
	if p.bind.cancel != nil {
		p.bind.cancel() // 旧流程作废，goroutine 见 ctx 取消后退出
	}
	ctx, cancel := context.WithTimeout(p.ctx, bindTimeout)
	gen := p.bind.gen + 1
	p.bind = bindState{status: plugin.ActionPending, message: "正在获取二维码…", cancel: cancel, gen: gen}
	apiBase := p.apiBase
	p.wg.Add(1)
	p.mu.Unlock()

	go p.bindFlow(ctx, cancel, gen, apiBase)
	return nil
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != "bind" {
		return plugin.ActionState{}, fmt.Errorf("没有操作 %q", key)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := plugin.ActionState{Status: p.bind.status, Message: p.bind.message, Image: p.bind.png, Link: p.bind.link}
	if st.Status == "" {
		st.Status = plugin.ActionIdle
	}
	return st, nil
}

// setBind 更新绑定流程状态（仅当本流程仍是当前代次时——旧流程被替换后不再发声）。
// png 与 link 是同一个二维码的两种呈现，成对更新。
func (p *Plugin) setBind(gen int, status, message string, png []byte, link string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bind.gen != gen {
		return
	}
	p.bind.status, p.bind.message = status, message
	if png != nil {
		p.bind.png, p.bind.link = png, link
	}
	if status == plugin.ActionDone || status == plugin.ActionError {
		p.bind.cancel = nil
		p.bind.png, p.bind.link = nil, "" // 结束后二维码不再有意义，不留在内存里
	}
}

// qrResponse 与 statusResponse 对照官方 login-qr.ts。
type qrResponse struct {
	Qrcode           string `json:"qrcode"`
	QrcodeImgContent string `json:"qrcode_img_content"` // 编码进二维码的 URL 字符串
}

type statusResponse struct {
	Status      string `json:"status"` // wait / scaned / confirmed / expired
	BotToken    string `json:"bot_token"`
	IlinkBotID  string `json:"ilink_bot_id"`
	BaseURL     string `json:"baseurl"`
	IlinkUserID string `json:"ilink_user_id"` // 扫码人
}

// bindFlow 是绑定流程主体：取二维码 → 轮询扫码状态（过期自动刷新）→ 持久化凭证并起轮询。
func (p *Plugin) bindFlow(ctx context.Context, cancel context.CancelFunc, gen int, apiBase string) {
	defer p.wg.Done()
	defer cancel()

	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		log.Printf("wechat_bot: 绑定失败: %s", msg)
		p.setBind(gen, plugin.ActionError, "绑定失败："+msg, nil, "")
	}

	qr, err := p.fetchQR(ctx, apiBase)
	if err != nil {
		fail("获取二维码出错: %v", err)
		return
	}
	png, err := qrcode.Encode(qr.QrcodeImgContent, qrcode.Medium, 256)
	if err != nil {
		fail("生成二维码图片出错: %v", err)
		return
	}
	p.setBind(gen, plugin.ActionPending, "请用微信扫码并确认（微信 App：我 → 设置 → 插件 → ClawBot）", png, qr.QrcodeImgContent)

	refreshes := 0
	for {
		if ctx.Err() != nil {
			p.setBind(gen, plugin.ActionError, "绑定超时或已取消，请重新发起。", nil, "")
			return
		}
		st, err := p.pollQRStatus(ctx, apiBase, qr.Qrcode)
		if err != nil {
			if ctx.Err() != nil {
				p.setBind(gen, plugin.ActionError, "绑定超时或已取消，请重新发起。", nil, "")
				return
			}
			fail("查询扫码状态出错: %v", err)
			return
		}
		switch st.Status {
		case "scaned":
			p.setBind(gen, plugin.ActionPending, "已扫码，请在微信中确认连接…", nil, "")
		case "expired":
			refreshes++
			if refreshes > maxQRRefresh {
				fail("二维码多次过期")
				return
			}
			qr, err = p.fetchQR(ctx, apiBase)
			if err != nil {
				fail("刷新二维码出错: %v", err)
				return
			}
			png, err = qrcode.Encode(qr.QrcodeImgContent, qrcode.Medium, 256)
			if err != nil {
				fail("生成二维码图片出错: %v", err)
				return
			}
			p.setBind(gen, plugin.ActionPending,
				fmt.Sprintf("二维码已过期，已自动刷新（%d/%d），请扫描新码", refreshes, maxQRRefresh), png, qr.QrcodeImgContent)
		case "confirmed":
			if st.BotToken == "" || st.BaseURL == "" {
				fail("服务端未返回完整凭证")
				return
			}
			creds := credentials{
				BotToken:     st.BotToken,
				BaseURL:      st.BaseURL,
				BotID:        st.IlinkBotID,
				BinderUserID: st.IlinkUserID,
			}
			p.mu.Lock()
			stateDir := p.stateDir
			p.mu.Unlock()
			if err := saveCredentials(stateDir, creds); err != nil {
				fail("保存凭证出错: %v", err)
				return
			}
			p.mu.Lock()
			p.creds = creds
			p.mu.Unlock()
			log.Printf("wechat_bot: 绑定成功，bot_id=%s 绑定人=%s", creds.BotID, creds.BinderUserID)
			p.setBind(gen, plugin.ActionDone,
				"✅ 绑定成功！绑定人 "+creds.BinderUserID+" 已自动放行，现在可以在微信里和助手对话了。", nil, "")
			p.startPolling()
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(qrPollInterval):
		}
	}
}

func (p *Plugin) fetchQR(ctx context.Context, apiBase string) (qrResponse, error) {
	var qr qrResponse
	raw, err := apiGet(ctx, apiBase, "/ilink/bot/get_bot_qrcode?bot_type=3", nil, apiTimeout)
	if err != nil {
		return qr, err
	}
	if err := json.Unmarshal(raw, &qr); err != nil {
		return qr, err
	}
	if qr.Qrcode == "" || qr.QrcodeImgContent == "" {
		return qr, fmt.Errorf("服务端未返回二维码")
	}
	return qr, nil
}

// pollQRStatus 长轮询一次扫码状态；客户端超时按 wait 处理（长轮询的正常情形）。
func (p *Plugin) pollQRStatus(ctx context.Context, apiBase, qrcodeID string) (statusResponse, error) {
	var st statusResponse
	raw, err := apiGet(ctx, apiBase, "/ilink/bot/get_qrcode_status?qrcode="+qrcodeID,
		map[string]string{"iLink-App-ClientVersion": "1"}, longPollTimeout)
	if err != nil {
		if ctx.Err() == nil && isTimeout(err) {
			return statusResponse{Status: "wait"}, nil
		}
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	return st, nil
}

// isTimeout 判定客户端侧超时（http 包会把超时包进 url.Error，按错误串兜底）。
func isTimeout(err error) bool {
	return err != nil && (errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "Client.Timeout"))
}
