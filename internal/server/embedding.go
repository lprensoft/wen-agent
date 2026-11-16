package server

import (
	"encoding/json"
	"net/http"

	"wen/internal/i18n"
	"wen/internal/llm"
	"wen/internal/modelcfg"
)

// 嵌入服务的接口。它是模型配置的一部分，但与聊天提供商分开：保存的内容、
// 测试的方式和生效的方式都不同——保存即生效，不经过 Agent，调用方每次现读配置。

// embeddingBody 是保存与测试共用的请求体。
type embeddingBody struct {
	Enabled    bool   `json:"enabled"`
	Type       string `json:"type"`
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions"`
}

func (b embeddingBody) embedding() modelcfg.Embedding {
	return modelcfg.Embedding{Enabled: b.Enabled, Type: b.Type, BaseURL: b.BaseURL,
		APIKey: b.APIKey, Model: b.Model, Dimensions: b.Dimensions}
}

// embeddingView 组装响应：api_key 一律不出现，只给掩码与「是否已配置」。
func (s *Server) embeddingView(lang string) map[string]any {
	e := s.models.Embedding()
	return map[string]any{
		"enabled":        e.Enabled,
		"type":           e.Type,
		"base_url":       e.BaseURL,
		"api_key_masked": modelcfg.MaskKey(e.APIKey),
		"has_api_key":    e.APIKey != "",
		"model":          e.Model,
		"dimensions":     e.Dimensions,
		"types":          modelcfg.EmbedTypeOptions(lang),
		"presets":        modelcfg.EmbeddingPresets(),
	}
}

func (s *Server) getEmbedding(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.embeddingView(reqLang(r)))
}

// saveEmbedding 保存嵌入配置，下一次嵌入调用即用新配置。
func (s *Server) saveEmbedding(w http.ResponseWriter, r *http.Request) {
	var req embeddingBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, err := s.models.SetEmbedding(reqLang(r), req.embedding()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.embeddingView(reqLang(r)))
}

// testEmbedding 用一次真实的嵌入请求验证地址、密钥与模型，返回实际的向量维度。
// 测的是表单上还没保存的内容，所以与保存是两个接口；密钥留空时沿用已保存的，
// 但地址换了就不沿用（理由见 modelcfg.InheritEmbeddingKey）。
func (s *Server) testEmbedding(w http.ResponseWriter, r *http.Request) {
	var req embeddingBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	lang := reqLang(r)
	e := modelcfg.NormalizeEmbedding(req.embedding())
	e = modelcfg.InheritEmbeddingKey(e, s.models.Embedding())
	// 测试不看「是否启用」：用户正是想在打开之前先试一下
	e.Enabled = true
	if err := modelcfg.ValidateEmbedding(lang, e); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	embedder, err := llm.NewEmbedder(e.LLMConfig(s.lang))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dims, err := llm.ProbeEmbed(r.Context(), embedder)
	if err != nil {
		writeError(w, http.StatusBadRequest, i18n.Tf(lang, "server.err.probe_failed", "连接失败：%s", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dimensions": dims})
}
