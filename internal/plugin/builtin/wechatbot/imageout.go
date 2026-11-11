package wechatbot

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"wen/internal/i18n"
	"wen/internal/llm"
)

// defCDNBase 是 iLink 媒体 CDN 的默认基址（与官方 SDK 一致）。
const defCDNBase = "https://novac2c.cdn.weixin.qq.com/c2c"

// 官方 UploadMediaType：1 图片、2 视频、3 文件、4 语音。
const (
	uploadMediaTypeImage = 1
	uploadMediaTypeFile  = 3
)

// sendImage 按官方 SDK 的流程发一张图：申请上传地址 → AES-128-ECB 加密后
// POST 到 CDN → 用 image_item 发消息。必须带 context_token 才能关联到对话窗口。
func (p *Plugin) sendImage(ctx context.Context, userID, contextToken string, img llm.ImagePart) error {
	creds, cdnBase, token, err := p.sendContext(userID, contextToken)
	if err != nil {
		return err
	}
	media, cipherSize, err := p.uploadMedia(ctx, creds, cdnBase, userID, uploadMediaTypeImage, img.FileName(), img.Data)
	if err != nil {
		return err
	}
	return p.sendItem(ctx, creds, userID, token, messageItem{
		Type: 2,
		ImageItem: &imageItem{
			Media:   media,
			MidSize: cipherSize,
		},
	})
}

// sendFile 与 sendImage 走同一条 CDN 上传链路，只是媒体类型与消息项不同。
func (p *Plugin) sendFile(ctx context.Context, userID, contextToken string, f llm.FilePart) error {
	creds, cdnBase, token, err := p.sendContext(userID, contextToken)
	if err != nil {
		return err
	}
	media, _, err := p.uploadMedia(ctx, creds, cdnBase, userID, uploadMediaTypeFile, f.FileName(), f.Data)
	if err != nil {
		return err
	}
	return p.sendItem(ctx, creds, userID, token, messageItem{
		Type: 4,
		FileItem: &fileItem{
			Media:    media,
			FileName: f.FileName(),
			Len:      fileLen(strconv.Itoa(len(f.Data))),
		},
	})
}

// sendContext 取发送用的凭据与 context_token。转投过来的媒体没有本轮凭据时，
// 用该用户最近一次入站留下的 token；仍然没有就明确失败。
func (p *Plugin) sendContext(userID, contextToken string) (credentials, string, string, error) {
	p.mu.Lock()
	creds, cdnBase, tokens := p.creds, p.cdnBase, p.tokens
	p.mu.Unlock()
	if !creds.valid() {
		return credentials{}, "", "", fmt.Errorf("%s", i18n.T(p.langOf(), "plugin.wechat_bot.err.media_unbound", "微信尚未绑定，无法发送媒体"))
	}
	if contextToken == "" && tokens != nil {
		contextToken = tokens.Get(userID)
	}
	if contextToken == "" {
		return credentials{}, "", "", fmt.Errorf("%s", i18n.T(p.langOf(), "plugin.wechat_bot.err.media_no_token", "缺少 context_token，无法发送"))
	}
	if p.paused() {
		return credentials{}, "", "", fmt.Errorf("%s", i18n.T(p.langOf(), "plugin.wechat_bot.err.media_paused", "微信侧 token 已失效，暂停发送"))
	}
	return creds, cdnBase, contextToken, nil
}

func (p *Plugin) uploadMedia(ctx context.Context, creds credentials, cdnBase, userID string, mediaType int, name string, data []byte) (cdnMedia, int, error) {
	aesKey := make([]byte, 16)
	fileKey := make([]byte, 16)
	if _, err := rand.Read(aesKey); err != nil {
		return cdnMedia{}, 0, err
	}
	if _, err := rand.Read(fileKey); err != nil {
		return cdnMedia{}, 0, err
	}
	keyHex := hex.EncodeToString(aesKey)
	fileKeyHex := hex.EncodeToString(fileKey)

	ciphertext := encryptECB(aesKey, data)
	sum := md5.Sum(data)
	uploadParam, fullURL, err := p.getUploadURL(ctx, creds, userID, fileKeyHex, keyHex,
		mediaType, len(data), len(ciphertext), hex.EncodeToString(sum[:]))
	if err != nil {
		return cdnMedia{}, 0, err
	}
	cdnURL := fullURL
	if cdnURL == "" {
		cdnURL = cdnBase + "/upload?encrypted_query_param=" + url.QueryEscape(uploadParam) +
			"&filekey=" + url.QueryEscape(fileKeyHex)
	}
	encryptedParam, err := uploadToCDN(ctx, cdnURL, ciphertext)
	if err != nil {
		return cdnMedia{}, 0, err
	}
	return cdnMedia{
		EncryptQueryParam: encryptedParam,
		AESKey:            base64.StdEncoding.EncodeToString([]byte(keyHex)),
		EncryptType:       1,
	}, len(ciphertext), nil
}

func (p *Plugin) sendItem(ctx context.Context, creds credentials, userID, contextToken string, item messageItem) error {
	body := map[string]any{
		"msg": wxMessage{
			ToUserID:     userID,
			ClientID:     randomClientID(),
			MessageType:  msgTypeBot,
			MessageState: stateFinish,
			ContextToken: contextToken,
			ItemList:     []messageItem{item},
		},
		"base_info": newBaseInfo(),
	}
	raw, err := apiPost(ctx, creds.BaseURL, "/ilink/bot/sendmessage", body, creds.BotToken, apiTimeout)
	if err != nil {
		return err
	}
	var resp struct {
		Ret     int    `json:"ret"`
		Errcode int    `json:"errcode"`
		Errmsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &resp); err == nil && (resp.Ret != 0 || resp.Errcode != 0) {
		return fmt.Errorf("sendmessage: ret=%d errcode=%d %s", resp.Ret, resp.Errcode, resp.Errmsg)
	}
	return nil
}

func (p *Plugin) getUploadURL(ctx context.Context, creds credentials, userID, fileKey, keyHex string, mediaType, rawSize, cipherSize int, md5Hex string) (string, string, error) {
	body := map[string]any{
		"filekey":       fileKey,
		"media_type":    mediaType,
		"to_user_id":    userID,
		"rawsize":       rawSize,
		"rawfilemd5":    md5Hex,
		"filesize":      cipherSize,
		"no_need_thumb": true,
		"aeskey":        keyHex,
		"base_info":     newBaseInfo(),
	}
	raw, err := apiPost(ctx, creds.BaseURL, "/ilink/bot/getuploadurl", body, creds.BotToken, apiTimeout)
	if err != nil {
		return "", "", err
	}
	var resp struct {
		Ret           int    `json:"ret"`
		Errcode       int    `json:"errcode"`
		Errmsg        string `json:"errmsg"`
		UploadParam   string `json:"upload_param"`
		UploadFullURL string `json:"upload_full_url"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", fmt.Errorf("getuploadurl 响应无法解析: %w", err)
	}
	if resp.Ret != 0 || resp.Errcode != 0 {
		return "", "", fmt.Errorf("getuploadurl: ret=%d errcode=%d %s", resp.Ret, resp.Errcode, resp.Errmsg)
	}
	if resp.UploadParam == "" && resp.UploadFullURL == "" {
		return "", "", fmt.Errorf("getuploadurl 没有返回上传地址")
	}
	return resp.UploadParam, resp.UploadFullURL, nil
}

func uploadToCDN(ctx context.Context, cdnURL string, ciphertext []byte) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, cdnURL, bytes.NewReader(ciphertext))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := resp.Header.Get("x-error-message")
		if msg == "" {
			msg = string(raw)
		}
		return "", fmt.Errorf("CDN 上传返回 HTTP %d: %s", resp.StatusCode, truncate(msg, 200))
	}
	param := resp.Header.Get("x-encrypted-param")
	if param == "" {
		return "", fmt.Errorf("CDN 上传响应缺少 x-encrypted-param")
	}
	return param, nil
}

// encryptECB 用 AES-128-ECB + PKCS#7 加密。Go 标准库没有 ECB 模式，按块手工调用。
func encryptECB(key, plain []byte) []byte {
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}
	return out
}
