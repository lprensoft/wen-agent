package wechatbot

import (
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/llm"
)

var errMediaTooLarge = errors.New("媒体超过大小上限")

// inboundContent 把 item_list 归一化成正文、图片与文件。图片与文件按官方 SDK
// 的规则下载：优先 media.full_url，否则用 cdnBase/download 拼；有密钥就做
// AES-128-ECB 解密，图片没有密钥时按明文下载。
func (p *Plugin) inboundContent(ctx context.Context, items []messageItem) (string, []llm.ImagePart, []llm.FilePart, string, bool) {
	var parts []string
	var images []llm.ImagePart
	var files []llm.FilePart
	var notes []string
	unsupported := false
	imageTotal, fileTotal := 0, 0
	tooManyImages, tooManyFiles := false, false

	for _, item := range items {
		switch item.Type {
		case 1:
			if item.TextItem != nil && strings.TrimSpace(item.TextItem.Text) != "" {
				parts = append(parts, item.TextItem.Text)
			}
		case 3:
			if item.VoiceItem != nil && strings.TrimSpace(item.VoiceItem.Text) != "" {
				parts = append(parts, i18n.T(p.langOf(), "plugin.wechat_bot.voice_prefix", "【语音】")+item.VoiceItem.Text)
			} else {
				unsupported = true
			}
		case 2:
			if item.ImageItem == nil {
				unsupported = true
				continue
			}
			if len(images) >= imbot.MaxImages {
				tooManyImages = true
				continue
			}
			img, err := p.inboundImage(ctx, item.ImageItem)
			if err != nil {
				notes = append(notes, p.mediaErrorNote(err, "image"))
				continue
			}
			if imageTotal+len(img.Data) > imbot.MaxImageTotal {
				notes = append(notes, i18n.T(p.langOf(), "plugin.wechat_bot.msg.image_too_large", "图片超过大小上限，请压缩后再发。"))
				continue
			}
			images = append(images, img)
			imageTotal += len(img.Data)
		case 4:
			if item.FileItem == nil {
				unsupported = true
				continue
			}
			if len(files) >= imbot.MaxFiles {
				tooManyFiles = true
				continue
			}
			f, err := p.inboundFile(ctx, item.FileItem)
			if err != nil {
				notes = append(notes, p.mediaErrorNote(err, "file"))
				continue
			}
			if fileTotal+len(f.Data) > imbot.MaxFileTotal {
				notes = append(notes, i18n.T(p.langOf(), "plugin.wechat_bot.msg.file_too_large", "文件超过大小上限，请压缩后再发。"))
				continue
			}
			files = append(files, f)
			fileTotal += len(f.Data)
		default:
			unsupported = true
		}
	}
	if tooManyImages {
		notes = append(notes, i18n.T(p.langOf(), "plugin.wechat_bot.msg.image_too_many",
			"一次最多处理 4 张图片，其余图片未发送给模型。"))
	}
	if tooManyFiles {
		notes = append(notes, i18n.T(p.langOf(), "plugin.wechat_bot.msg.file_too_many",
			"一次最多处理 4 个文件，其余文件未发送给模型。"))
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), images, files, strings.Join(notes, "\n"), unsupported
}

func (p *Plugin) mediaErrorNote(err error, kind string) string {
	lang := p.langOf()
	if errors.Is(err, errMediaTooLarge) {
		if kind == "image" {
			return i18n.T(lang, "plugin.wechat_bot.msg.image_too_large", "图片超过大小上限，请压缩后再发。")
		}
		return i18n.T(lang, "plugin.wechat_bot.msg.file_too_large", "文件超过大小上限，请压缩后再发。")
	}
	if kind == "image" {
		i18n.Logf(lang, "log.wechat.image_failed", "wechat_bot: 下载或解密图片失败: %v", err)
		return i18n.T(lang, "plugin.wechat_bot.msg.image_failed", "图片下载失败，请重发。")
	}
	i18n.Logf(lang, "log.wechat.file_failed", "wechat_bot: 下载或解密文件失败: %v", err)
	return i18n.T(lang, "plugin.wechat_bot.msg.file_failed", "文件下载失败，请重发。")
}

func (p *Plugin) inboundImage(ctx context.Context, item *imageItem) (llm.ImagePart, error) {
	key, err := imageKey(item)
	if err != nil {
		return llm.ImagePart{}, err
	}
	data, err := p.downloadCDN(ctx, item.Media, key, imbot.MaxImageBytes)
	if err != nil {
		return llm.ImagePart{}, err
	}
	img, err := imbot.ImageFromBytes(data)
	if err != nil {
		i18n.Logf(p.langOf(), "log.wechat.image_invalid", "wechat_bot: 图片无效（已跳过）: %v", err)
		return llm.ImagePart{}, err
	}
	return img, nil
}

// imageKey 按官方 SDK 的顺序取密钥：image_item.aeskey（hex）优先，其次
// media.aes_key（Base64）。两者都没有时返回 nil，表示按明文下载。
func imageKey(item *imageItem) ([]byte, error) {
	if raw := strings.TrimSpace(item.AESKey); raw != "" {
		if key, err := hex.DecodeString(raw); err == nil && len(key) == aes.BlockSize {
			return key, nil
		}
	}
	if raw := strings.TrimSpace(item.Media.AESKey); raw != "" {
		return parseBase64Key(raw)
	}
	return nil, nil
}

func (p *Plugin) inboundFile(ctx context.Context, item *fileItem) (llm.FilePart, error) {
	if item.Media.EncryptQueryParam == "" && item.Media.FullURL == "" {
		return llm.FilePart{}, errors.New("文件缺少下载地址")
	}
	key, err := parseBase64Key(item.Media.AESKey)
	if err != nil {
		return llm.FilePart{}, err
	}
	data, err := p.downloadCDN(ctx, item.Media, key, imbot.MaxFileBytes)
	if err != nil {
		return llm.FilePart{}, err
	}
	name := strings.TrimSpace(item.FileName)
	if name == "" {
		name = fileNameFromURL(item.Media.FullURL)
	}
	f, err := imbot.FileFromBytes(data, name, "")
	if err != nil {
		i18n.Logf(p.langOf(), "log.wechat.file_invalid", "wechat_bot: 文件无效（已跳过）: %v", err)
		return llm.FilePart{}, err
	}
	return f, nil
}

// parseBase64Key 解出 16 字节密钥。官方线上有两种编码：base64(16 字节) 与
// base64(32 位十六进制字符串)。
func parseBase64Key(raw string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("aes_key 不是合法的 Base64: %w", err)
	}
	if len(decoded) == aes.BlockSize {
		return decoded, nil
	}
	if len(decoded) == 2*aes.BlockSize {
		if key, err := hex.DecodeString(string(decoded)); err == nil && len(key) == aes.BlockSize {
			return key, nil
		}
	}
	return nil, fmt.Errorf("aes_key 长度无效: %d", len(decoded))
}

func (p *Plugin) downloadCDN(ctx context.Context, media cdnMedia, key []byte, max int) ([]byte, error) {
	target := strings.TrimSpace(media.FullURL)
	if target == "" {
		if media.EncryptQueryParam == "" {
			return nil, errors.New("媒体缺少下载参数")
		}
		p.mu.Lock()
		base := p.cdnBase
		p.mu.Unlock()
		if base == "" {
			base = defCDNBase
		}
		target = base + "/download?encrypted_query_param=" + url.QueryEscape(media.EncryptQueryParam)
	}

	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDN 下载返回 HTTP %d", resp.StatusCode)
	}

	limit := int64(max + 1)
	if key != nil {
		limit = int64(max + aes.BlockSize + 1)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if key == nil {
		if len(body) > max {
			return nil, errMediaTooLarge
		}
		return body, nil
	}
	if len(body) > max+aes.BlockSize {
		return nil, errMediaTooLarge
	}
	plain, err := decryptECB(key, body)
	if err != nil {
		return nil, err
	}
	if len(plain) > max {
		return nil, errMediaTooLarge
	}
	return plain, nil
}

func decryptECB(key, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errors.New("密文长度无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(plain[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}
	pad := int(plain[len(plain)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(plain) {
		return nil, errors.New("PKCS#7 填充无效")
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, errors.New("PKCS#7 填充无效")
		}
	}
	return plain[:len(plain)-pad], nil
}

func fileNameFromURL(raw string) string {
	if raw == "" {
		return "file"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "file"
	}
	if base := path.Base(u.Path); base != "" && base != "." && base != "/" {
		return base
	}
	return "file"
}
