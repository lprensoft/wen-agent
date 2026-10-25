package wechatbot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wen/internal/version"
)

// iLink Bot API 的 HTTP 封装。所有接口在同一基址下（扫码用公共基址，绑定后用
// 服务端下发的专属 baseurl），JSON over HTTP，对照官方 openclaw-weixin 包移植。

// channelVersion 随每个请求的 base_info 上报，取官方渠道包的版本号。协议行为已对齐
// 到这一版（扫码改 POST、公共请求头、无符号消息 ID、-14 暂停全部请求）。
const channelVersion = "2.4.10"

// iLinkAppID 是公共请求头 iLink-App-Id 的值，与官方包一致。
const iLinkAppID = "bot"

// clientVersionHeader 是 iLink-App-ClientVersion 的值：版本号按 0x00MMNNPP 编码后的十进制。
var clientVersionHeader = strconv.Itoa(encodeClientVersion(channelVersion))

func encodeClientVersion(v string) int {
	var parts [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(s)
		parts[i] = n & 0xff
	}
	return parts[0]<<16 | parts[1]<<8 | parts[2]
}

const (
	longPollTimeout = 35 * time.Second // getupdates / 扫码状态长轮询的客户端超时
	apiTimeout      = 15 * time.Second // 普通接口超时
)

type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
	// BotAgent 只用于对方的日志与监控归因，不参与鉴权或路由；格式是 Name/Version
	BotAgent string `json:"bot_agent,omitempty"`
}

func newBaseInfo() baseInfo {
	return baseInfo{ChannelVersion: channelVersion, BotAgent: "Wen/" + version.Version}
}

// ---------- 报文结构（bytes 字段在 JSON 中是 base64 字符串） ----------

type textItem struct {
	Text string `json:"text"`
}

type voiceItem struct {
	Text string `json:"text,omitempty"` // 语音转文字内容（微信侧自带转写）
}

type messageItem struct {
	Type      int        `json:"type"` // 1=文本 2=图片 3=语音 4=文件 5=视频
	TextItem  *textItem  `json:"text_item,omitempty"`
	VoiceItem *voiceItem `json:"voice_item,omitempty"`
}

// 消息发起方与状态。
const (
	msgTypeUser = 1 // 用户发出
	msgTypeBot  = 2 // Bot 发出
	stateFinish = 2 // 完整消息（非流式）
)

type wxMessage struct {
	// MessageID 线上是 uint64（官方 2.4.9 起按无损方式解析）：按有符号数解析的话，
	// 一旦有 ID 超过 2^63，整个 getupdates 响应解不开，游标不再前进，收信就卡死了
	MessageID    uint64        `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	GroupID      string        `json:"group_id,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
	MessageState int           `json:"message_state,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
	ItemList     []messageItem `json:"item_list,omitempty"`
}

type getUpdatesResp struct {
	Ret                  int         `json:"ret"`
	Errcode              int         `json:"errcode"`
	Errmsg               string      `json:"errmsg"`
	Msgs                 []wxMessage `json:"msgs"`
	GetUpdatesBuf        string      `json:"get_updates_buf"`
	LongpollingTimeoutMs int         `json:"longpolling_timeout_ms"`
}

// errcodeExpired 是服务端「token 失效」错误码（官方叫 STALE_TOKEN），收到后暂停
// **全部**请求一段时间，发送也不例外（官方行为）。
const errcodeExpired = -14

// ---------- HTTP ----------

// randomUIN 生成 X-WECHAT-UIN 头：随机 uint32 → 十进制字符串 → base64，每次请求都变。
func randomUIN() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	n := binary.BigEndian.Uint32(b[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(n), 10)))
}

// apiPost 向 iLink 接口发一个 JSON POST。token 为空时不带 Authorization（扫码前）。
func apiPost(ctx context.Context, base, endpoint string, body any, token string, timeout time.Duration) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, base+endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomUIN())
	setAppHeaders(req)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d: %s", endpoint, resp.StatusCode, truncate(string(data), 200))
	}
	return data, nil
}

// setAppHeaders 补上官方要求每个请求都带的应用请求头。
func setAppHeaders(req *http.Request) {
	req.Header.Set("iLink-App-Id", iLinkAppID)
	req.Header.Set("iLink-App-ClientVersion", clientVersionHeader)
}

// apiGet 发一个 GET（扫码状态接口）：只带应用请求头，不带鉴权、UIN 与 AuthorizationType。
func apiGet(ctx context.Context, base, endpoint string, headers map[string]string, timeout time.Duration) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, base+endpoint, nil)
	if err != nil {
		return nil, err
	}
	setAppHeaders(req)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d: %s", endpoint, resp.StatusCode, truncate(string(data), 200))
	}
	return data, nil
}

// randomClientID 生成发送消息的 client_id。
func randomClientID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "wen-" + base64.RawURLEncoding.EncodeToString(b[:])
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
