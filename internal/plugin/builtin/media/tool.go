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
	"wen/internal/pathsafe"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

type sendImageTool struct{ p *Plugin }

func (t *sendImageTool) Name() string { return "send_image" }

func (t *sendImageTool) Description() string {
	return prompt.T(t.p.lang, "prompt.media.tool.send_image.desc",
		"把工作目录里的一张图片发给当前正在交谈的人。只支持 IM 通道；图片须为 PNG 或 JPEG，单张不超过 5 MB。发送成功后本轮会收到简短确认，可以在之后的回复里继续说话。")
}

const schemaZH = `{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "图片路径，相对工作目录或工作目录内的绝对路径"}
		},
		"required": ["path"]
	}`

func (t *sendImageTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.lang, "prompt.media.tool.send_image.schema", schemaZH))
}

func (t *sendImageTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	lang := t.p.lang
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(lang, err)
	}
	path, err := t.p.resolve(a.Path, imageErrKeys)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s", i18n.Tf(lang, "plugin.media.err.not_file", "「%s」不是可读取的图片文件。", strings.TrimSpace(a.Path)))
	}
	if info.Size() > imbot.MaxImageBytes {
		return "", fmt.Errorf("%s", i18n.Tf(lang, "plugin.media.err.too_large",
			"图片超过 %d MB，未发送。", imbot.MaxImageBytes>>20))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		i18n.Logf(lang, "log.media.read_failed", "media: 读取图片失败: %v", err)
		return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.err.read", "读取图片失败。"))
	}
	img, err := imbot.ImageFromBytes(data)
	if err != nil {
		i18n.Logf(lang, "log.media.image_invalid", "media: 图片校验失败: %v", err)
		return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.err.invalid", "不是可用的 PNG / JPEG / GIF 图片。"))
	}
	img.Name = filepath.Base(path)

	if err := imbot.SendTurnImage(ctx, img); err != nil {
		switch {
		case errors.Is(err, imbot.ErrNoMediaChannel):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.err.no_channel",
				"当前对话不是从 IM 通道发起的，无法发送图片。"))
		case errors.Is(err, imbot.ErrMediaTargetUnavailable):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.err.target_unavailable",
				"图片要发往的通道当前不可用，未发送。"))
		case errors.Is(err, imbot.ErrMediaDeliveryUnknown):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.err.delivery_unknown",
				"图片可能已送达，但平台没有确认，请勿重复发送。"))
		case errors.Is(err, imbot.ErrMediaUnsupported):
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.err.unsupported",
				"这条通道不支持发送图片。"))
		default:
			// 平台错误原文可能混着另一种语言的句子，只进日志，不进模型上下文。
			i18n.Logf(lang, "log.media.send_failed", "media: 发送图片失败: %v", err)
			return "", fmt.Errorf("%s", i18n.T(lang, "plugin.media.err.send", "发送图片失败，请检查通道配置。"))
		}
	}
	return i18n.Tf(lang, "plugin.media.sent", "已发送图片「%s」。", img.Name), nil
}

// mediaErrKeys 是路径校验用的一组文案键与中文里的称呼。
type mediaErrKeys struct {
	noun      string // 中文里的「图片」「文件」
	emptyPath string
	noWorkdir string
	notFound  string
	outside   string
}

var (
	imageErrKeys = mediaErrKeys{
		noun:      "图片",
		emptyPath: "plugin.media.err.empty_path",
		noWorkdir: "plugin.media.err.no_workdir",
		notFound:  "plugin.media.err.not_found",
		outside:   "plugin.media.err.outside_workdir",
	}
	fileErrKeys = mediaErrKeys{
		noun:      "文件",
		emptyPath: "plugin.media.file.err.empty_path",
		noWorkdir: "plugin.media.file.err.no_workdir",
		notFound:  "plugin.media.file.err.not_found",
		outside:   "plugin.media.file.err.outside_workdir",
	}
)

// resolve 把工具参数变成工作目录内的真实路径。符号链接先解析再判断归属，
// 否则目录里一个指向 .env 之类文件的链接就能绕过限制。
func (p *Plugin) resolve(raw string, k mediaErrKeys) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%s", i18n.T(p.lang, k.emptyPath, "请给出"+k.noun+"路径。"))
	}
	if p.workdir == "" {
		return "", fmt.Errorf("%s", i18n.T(p.lang, k.noWorkdir, "没有可用的工作目录。"))
	}
	root, err := filepath.Abs(p.workdir)
	if err != nil {
		return "", fmt.Errorf("%s", i18n.T(p.lang, k.noWorkdir, "没有可用的工作目录。"))
	}
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	real, err := pathsafe.Real(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("%s", i18n.Tf(p.lang, k.notFound, "找不到"+k.noun+"「%s」。", raw))
	}
	// pathsafe 会跟随 Windows 的目录联接；只用 EvalSymlinks 的话联接能绕过限制。
	if !pathsafe.Within(root, real) {
		return "", fmt.Errorf("%s", i18n.Tf(p.lang, k.outside,
			"只能发送工作目录内的"+k.noun+"：「%s」在工作目录之外。", raw))
	}
	return real, nil
}
