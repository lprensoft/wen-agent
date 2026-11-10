package imbot

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"wen/internal/llm"
)

// 入站图片的统一上限。各通道下载前先看平台给的大小，下载后再按实际字节复核。
const (
	MaxImages     = 4
	MaxImageBytes = 5 << 20
	MaxImageTotal = 10 << 20
	maxPixels     = 25_000_000
)

// ImageFromBytes 校验并归一化一张入站图片。只看文件头不够：签名正确、内容
// 损坏的图会在模型解码时失败，而它已经随消息落盘；因此这里完整解码一次。
func ImageFromBytes(data []byte) (llm.ImagePart, error) {
	if len(data) == 0 {
		return llm.ImagePart{}, fmt.Errorf("图片内容为空")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return llm.ImagePart{}, fmt.Errorf("图片无法解码或已损坏")
	}
	if cfg.Width*cfg.Height > maxPixels {
		return llm.ImagePart{}, fmt.Errorf("图片尺寸过大")
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return llm.ImagePart{}, fmt.Errorf("图片无法解码或已损坏")
	}
	var mediaType string
	switch format {
	case "png":
		mediaType = "image/png"
	case "jpeg":
		mediaType = "image/jpeg"
	case "gif":
		mediaType = "image/gif"
	default:
		return llm.ImagePart{}, fmt.Errorf("不支持的图片格式 %s", format)
	}
	return llm.ImagePart{MediaType: mediaType, Size: len(data), Data: data}, nil
}
