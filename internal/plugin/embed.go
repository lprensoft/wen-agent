package plugin

import (
	"context"
	"errors"
)

// 文本向量化是个通用机制：核心只提供「把一批文本变成向量」，不知道调用方拿它做记忆
// 召回还是别的。配置在核心那一侧（models.json 的 embedding 段），插件无从自行选择服务，
// 也就不会各配一遍。

// ErrEmbedUnavailable 表示当前没有启用嵌入服务。它是常态而不是故障——嵌入默认关闭，
// 插件据此静默降级，不该为它记日志或向用户报错。
var ErrEmbedUnavailable = errors.New("embedding is not enabled")

// EmbedResult 是一次嵌入的结果。
type EmbedResult struct {
	// Model 是这批向量的身份（模型、维度等）。身份不同的向量不能放在一起比较，
	// 落盘的索引要连同它一起存：随结果交回而不是另问一次，是因为配置可能在两次
	// 调用之间被改掉，另问就可能把新身份贴到旧向量上。
	Model string
	// Vectors 与输入一一对应，每个向量已归一化，点积即余弦相似度。
	Vectors [][]float32
}

// EmbedFunc 见 InitContext.Embed。
type EmbedFunc func(ctx context.Context, texts []string) (EmbedResult, error)

// EmbedModelFunc 见 InitContext.EmbedModel。
type EmbedModelFunc func() string
