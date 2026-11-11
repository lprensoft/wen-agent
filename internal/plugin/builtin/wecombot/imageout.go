package wecombot

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"wen/internal/i18n"
	"wen/internal/imbot"
	"wen/internal/llm"
)

// 长连接素材上传的三个命令（官方 path/101838）。
const (
	cmdUploadInit   = "aibot_upload_media_init"
	cmdUploadChunk  = "aibot_upload_media_chunk"
	cmdUploadFinish = "aibot_upload_media_finish"

	// 单个分片不超过 512 KB（Base64 编码前），最多 100 片。
	uploadChunkSize = 512 << 10

	// 分片上传的每一步都等回执；比普通回复给得宽一些，避免大图在慢网络上被误判超时。
	uploadWait = 15 * time.Second
)

// sendImage 先分片上传拿 media_id，再发一条图片消息。
func (p *Plugin) sendImage(ctx context.Context, userID, replyTo string, img llm.ImagePart) error {
	mediaID, err := p.uploadMedia(ctx, "image", img.FileName(), img.Data)
	if err != nil {
		return err
	}
	return p.sendMedia(ctx, userID, replyTo, map[string]any{
		"msgtype": "image",
		"image":   map[string]any{"media_id": mediaID},
	}, func() error {
		return fmt.Errorf("%w: %s", imbot.ErrMediaDeliveryUnknown,
			i18n.Tf(p.langOf(), "plugin.wecom_bot.err.image_unconfirmed",
				"%s可能已送达，但平台没有确认（等待回执超时），请勿重复发送",
				i18n.T(p.langOf(), "plugin.wecom_bot.media.image", "图片")))
	})
}

// sendFile 与 sendImage 走同一条分片上传链路，msgtype 换成 file。
func (p *Plugin) sendFile(ctx context.Context, userID, replyTo string, f llm.FilePart) error {
	mediaID, err := p.uploadMedia(ctx, "file", f.FileName(), f.Data)
	if err != nil {
		return err
	}
	return p.sendMedia(ctx, userID, replyTo, map[string]any{
		"msgtype": "file",
		"file":    map[string]any{"media_id": mediaID},
	}, func() error {
		return fmt.Errorf("%w: %s", imbot.ErrMediaDeliveryUnknown,
			i18n.Tf(p.langOf(), "plugin.wecom_bot.err.file_unconfirmed",
				"%s可能已送达，但平台没有确认（等待回执超时），请勿重复发送",
				i18n.T(p.langOf(), "plugin.wecom_bot.media.file", "文件")))
	})
}

// sendMedia 有回调用过的 req_id 就被动回复，否则主动推送。回执超时视为可能
// 已送达，不再重发；三条路径都要报成「送达状态未知」。
func (p *Plugin) sendMedia(ctx context.Context, userID, replyTo string, body map[string]any, unconfirmed func() error) error {
	w := p.connSnapshot()
	if w == nil {
		return errNoConn
	}
	pushActive := func() error {
		body["chatid"] = userID
		body["chat_type"] = 1
		reqID := newReqID(cmdSend)
		err := w.call(ctx, reqID, outboundFrame{Cmd: cmdSend, Headers: frameHeaders{ReqID: reqID}, Body: body}, p.replyWait())
		if errors.Is(err, errReplyTimeout) {
			return unconfirmed()
		}
		return err
	}
	if replyTo == "" {
		return pushActive()
	}

	err := w.call(ctx, replyTo, outboundFrame{Cmd: cmdRespond, Headers: frameHeaders{ReqID: replyTo}, Body: body}, p.replyWait())
	switch {
	case errors.Is(err, errReqIDStale):
		// req_id 上有没确认的旧回执，改走主动推送
		return pushActive()
	case errors.Is(err, errReplyTimeout):
		return unconfirmed()
	}
	return err
}

func (p *Plugin) uploadMedia(ctx context.Context, kind, filename string, data []byte) (string, error) {
	w := p.connSnapshot()
	if w == nil {
		return "", errNoConn
	}
	sum := md5.Sum(data)
	totalChunks := (len(data) + uploadChunkSize - 1) / uploadChunkSize
	if totalChunks == 0 {
		totalChunks = 1
	}
	if totalChunks > 100 {
		return "", fmt.Errorf("文件过大，无法上传")
	}

	initID := newReqID(cmdUploadInit)
	resp, err := w.request(ctx, initID, outboundFrame{
		Cmd:     cmdUploadInit,
		Headers: frameHeaders{ReqID: initID},
		Body: map[string]any{
			"type":         kind,
			"filename":     filename,
			"total_size":   len(data),
			"total_chunks": totalChunks,
			"md5":          hex.EncodeToString(sum[:]),
		},
	}, uploadWait)
	if err != nil {
		return "", err
	}
	if resp.errcode != 0 {
		return "", fmt.Errorf("上传初始化失败: errcode=%d %s", resp.errcode, resp.errmsg)
	}
	var initBody struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(resp.body, &initBody); err != nil || initBody.UploadID == "" {
		return "", fmt.Errorf("上传初始化没有返回 upload_id")
	}

	for i := 0; i < totalChunks; i++ {
		start := i * uploadChunkSize
		end := min(start+uploadChunkSize, len(data))
		reqID := newReqID(cmdUploadChunk)
		r, err := w.request(ctx, reqID, outboundFrame{
			Cmd:     cmdUploadChunk,
			Headers: frameHeaders{ReqID: reqID},
			Body: map[string]any{
				"upload_id":   initBody.UploadID,
				"chunk_index": i,
				"base64_data": base64.StdEncoding.EncodeToString(data[start:end]),
			},
		}, uploadWait)
		if err != nil {
			return "", err
		}
		if r.errcode != 0 {
			return "", fmt.Errorf("上传分片 %d 失败: errcode=%d %s", i, r.errcode, r.errmsg)
		}
	}

	finishID := newReqID(cmdUploadFinish)
	resp, err = w.request(ctx, finishID, outboundFrame{
		Cmd:     cmdUploadFinish,
		Headers: frameHeaders{ReqID: finishID},
		Body:    map[string]any{"upload_id": initBody.UploadID},
	}, uploadWait)
	if err != nil {
		return "", err
	}
	if resp.errcode != 0 {
		return "", fmt.Errorf("完成上传失败: errcode=%d %s", resp.errcode, resp.errmsg)
	}
	var finishBody struct {
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(resp.body, &finishBody); err != nil || finishBody.MediaID == "" {
		return "", fmt.Errorf("完成上传没有返回 media_id")
	}
	return finishBody.MediaID, nil
}
