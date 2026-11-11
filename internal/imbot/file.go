package imbot

import (
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"wen/internal/llm"
)

// 入站文件的统一上限。取值对齐各平台里最紧的几个（企业微信、钉钉 20 MB，
// Telegram 下载 20 MB），这样同一份文件在九条通道上的行为一致。
const (
	MaxFiles     = 4
	MaxFileBytes = 20 << 20
	MaxFileTotal = 40 << 20
)

// FileFromBytes 校验并归一化一个入站文件。与图片不同，不检查内容能否解析：
// 任意字节都是合法文件。MIME 优先用通道给的，其次按扩展名推断，最后嗅探内容。
func FileFromBytes(data []byte, name, mediaType string) (llm.FilePart, error) {
	if len(data) == 0 {
		return llm.FilePart{}, fmt.Errorf("文件内容为空")
	}
	if len(data) > MaxFileBytes {
		return llm.FilePart{}, fmt.Errorf("文件超过 %d MB", MaxFileBytes>>20)
	}
	f := llm.FilePart{Name: name, Size: len(data), Data: data}
	f.Name = f.FileName()
	f.MediaType = normalizeMediaType(mediaType, f.Name, data)
	return f, nil
}

func normalizeMediaType(raw, name string, data []byte) string {
	raw = strings.TrimSpace(raw)
	if raw != "" && raw != "application/octet-stream" {
		return raw
	}
	if ext := filepath.Ext(name); ext != "" {
		if mt := mime.TypeByExtension(strings.ToLower(ext)); mt != "" {
			return mt
		}
	}
	return http.DetectContentType(data)
}
