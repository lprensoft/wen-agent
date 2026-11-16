package main

import (
	"context"

	"wen/internal/llm"
	"wen/internal/modelcfg"
	"wen/internal/plugin"
)

// embedFuncs 把模型配置里的嵌入服务接成插件可用的两个闭包。
//
// 每次调用都现读配置：设置页改了嵌入服务之后，下一次调用就用新的，不需要重启，
// 也不需要把正在用它的插件重新 Init。models 用函数取值，是因为插件先于模型配置构造。
func embedFuncs(lang string, models func() *modelcfg.Store) (plugin.EmbedFunc, plugin.EmbedModelFunc) {
	active := func() (llm.EmbedConfig, bool) {
		store := models()
		if store == nil {
			return llm.EmbedConfig{}, false
		}
		emb := store.Embedding()
		if !emb.Active() {
			return llm.EmbedConfig{}, false
		}
		return emb.LLMConfig(lang), true
	}
	embed := func(ctx context.Context, texts []string) (plugin.EmbedResult, error) {
		cfg, ok := active()
		if !ok {
			return plugin.EmbedResult{}, plugin.ErrEmbedUnavailable
		}
		e, err := llm.NewEmbedder(cfg)
		if err != nil {
			return plugin.EmbedResult{}, err
		}
		vecs, err := e.Embed(ctx, texts)
		if err != nil {
			return plugin.EmbedResult{}, err
		}
		return plugin.EmbedResult{Model: cfg.Identity(), Vectors: vecs}, nil
	}
	model := func() string {
		if cfg, ok := active(); ok {
			return cfg.Identity()
		}
		return ""
	}
	return embed, model
}
