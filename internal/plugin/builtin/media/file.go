package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

type sendFileTool struct{ p *Plugin }

func (t *sendFileTool) Name() string { return "send_file" }

func (t *sendFileTool) Description() string {
	return prompt.T(t.p.lang, "prompt.media.tool.send_file.desc",
		"把工作目录里的一个文件发给当前正在交谈的人。只支持 IM 通道；单个文件不超过 20 MB。发送成功后本轮会收到简短确认，可以在之后的回复里继续说话。")
}

const fileSchemaZH = `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "文件路径，相对工作目录或工作目录内的绝对路径"}
		},
		"required": ["path"]
	}`

func (t *sendFileTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.lang, "prompt.media.tool.send_file.schema", fileSchemaZH))
}

func (t *sendFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	lang := t.p.lang
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	path, err := t.p.resolve(a.Path, fileErrKeys)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s", i18n.Tf(lang, "plugin.media.file.err.not_file", "「%s」不是可读取的文件。", strings.TrimSpace(a.Path)))
	}
	if info.Size() > imbot.MaxFileBytes {
		return "", fmt.Errorf("%s", i18n.Tf(lang, "plugin.media.file.err.too_large",
			"文件超过 %d MB，未发送。", imbot.MaxFileBytes>>20))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		i18n.Logf(lang, "log.media.read_file_failed", "media: 读取文件失败: %v", err)
		return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.file.err.read", "读取文件失败。"))
	}
	f, err := imbot.FileFromBytes(data, filepath.Base(path), "")
	if err != nil {
		i18n.Logf(lang, "log.media.file_invalid", "media: 文件校验失败: %v", err)
		return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.file.err.invalid", "文件内容为空或超过限制。"))
	}

	if err := imbot.SendTurnFile(ctx, f); err != nil {
		switch {
		case errors.Is(err, imbot.ErrNoMediaChannel):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.file.err.no_channel",
				"当前对话不是从 IM 通道发起的，无法发送文件。"))
		case errors.Is(err, imbot.ErrMediaTargetUnavailable):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.file.err.target_unavailable",
				"文件要发往的通道当前不可用，未发送。"))
		case errors.Is(err, imbot.ErrMediaDeliveryUnknown):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.file.err.delivery_unknown",
				"文件可能已送达，但平台没有确认，请勿重复发送。"))
		case errors.Is(err, imbot.ErrMediaUnsupported):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.file.err.unsupported",
				"这条通道不支持发送文件。"))
		default:
			i18n.Logf(lang, "log.media.send_file_failed", "media: 发送文件失败: %v", err)
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.file.err.send", "发送文件失败，请检查通道配置。"))
		}
	}
	return i18n.Tf(lang, "plugin.media.file.sent", "已发送文件「%s」。", f.FileName()), nil
}
