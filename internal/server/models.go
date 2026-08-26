package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"wen/internal/agent"
	"wen/internal/llm"
	"wen/internal/modelcfg"

	"wen/internal/i18n"
)

// listModels 返回提供商与模型的全量配置（API Key 只给掩码）。
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.modelsView(reqLang(r)))
}

// saveModels 整档保存配置：校验通过后落盘，并让当前选中的模型立即生效。
func (s *Server) saveModels(w http.ResponseWriter, r *http.Request) {
	var req modelcfg.File
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, err := s.models.Save(reqLang(r), req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.applyCurrentModel(reqLang(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.modelsView(reqLang(r)))
}

// setCurrentModel 切换当前使用的提供商与模型。
func (s *Server) setCurrentModel(w http.ResponseWriter, r *http.Request) {
	var sel modelcfg.Selection
	if err := json.NewDecoder(r.Body).Decode(&sel); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, err := s.models.SetCurrent(reqLang(r), sel); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.applyCurrentModel(reqLang(r)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.modelsView(reqLang(r)))
}

// testModel 用极小的一次真实请求验证 base_url / API Key / 模型 id 是否可用。
func (s *Server) testModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Type     string `json:"type"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Dialect  string `json:"thinking_dialect"`
		Model    string `json:"model"`
		// PromptCache 未设置按开启算，与 modelcfg.Resolved 的口径一致
		PromptCache *bool `json:"prompt_cache"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "请先为该提供商添加模型后再测试")
		return
	}
	// 密钥留空表示沿用已保存的值（界面上只拿得到掩码）
	if req.APIKey == "" {
		if p, _, ok := s.models.Lookup(req.Provider, req.Model); ok {
			req.APIKey = p.APIKey
		}
	}
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "server.err.need_api_key", "请填写 API Key"))
		return
	}

	provider, err := llm.New(llm.Config{
		Type: req.Type, BaseURL: req.BaseURL, APIKey: req.APIKey, Dialect: req.Dialect,
		PromptCache: req.PromptCache == nil || *req.PromptCache,
		Lang:        s.lang,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := probe(r.Context(), provider, req.Model); err != nil {
		writeError(w, http.StatusBadRequest, "连接失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// probe 发起一次最小请求，收到首个事件即视为连通。
func probe(ctx context.Context, provider llm.Provider, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	events, err := provider.ChatStream(ctx, llm.ChatRequest{
		Model:     model,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "ping"}},
		MaxTokens: 16,
		Thinking:  "off",
	})
	if err != nil {
		return err
	}
	// 收到首个事件即视为连通，随后 cancel 关闭剩余的流
	ev, ok := <-events
	if !ok {
		return fmt.Errorf("模型没有返回任何内容")
	}
	if ev.Type == llm.EventError {
		return ev.Err
	}
	return nil
}

// applyCurrentModel 按当前选中项重建 provider 并热切换到 Agent。
func (s *Server) applyCurrentModel(lang string) error {
	cur, err := s.models.Resolve(lang)
	if err != nil {
		return err
	}
	provider, err := llm.New(llm.Config{
		Type: cur.Type, BaseURL: cur.BaseURL, APIKey: cur.APIKey, Dialect: cur.Dialect,
		PromptCache: cur.PromptCache,
		Lang:        s.lang,
	})
	if err != nil {
		return err
	}
	s.agent.SetModel(provider, agent.ModelOptions{
		Model:         cur.ModelID,
		Temperature:   cur.Temperature,
		MaxTokens:     cur.MaxTokens,
		Thinking:      cur.Thinking,
		ContextLength: cur.ContextLength,
	})
	return nil
}

// modelsView 组装响应：api_key 一律不出现，只给掩码与「是否已配置」。
func (s *Server) modelsView(lang string) map[string]any {
	view := s.models.Snapshot()
	def := s.models.Defaults()

	providers := make([]map[string]any, 0, len(view.Providers))
	for _, p := range view.Providers {
		models := p.Models
		if models == nil {
			models = []modelcfg.Model{}
		}
		providers = append(providers, map[string]any{
			"name":             p.Name,
			"type":             p.Type,
			"base_url":         p.BaseURL,
			"api_key_masked":   modelcfg.MaskKey(p.APIKey),
			"has_api_key":      p.APIKey != "",
			"thinking_dialect": p.Dialect,
			// 三态：true / false / null（未设置，按开启算）。界面据此区分「用户明确关过」
			// 与「从未设置」，后者保存时仍写回 null，免得一次保存就把条目从 config.yaml 接管过来。
			"prompt_cache": p.PromptCache,
			"source":       p.Source,
			"models":       models,
		})
	}
	return map[string]any{
		"providers": providers,
		"current":   view.Current,
		"types":     modelcfg.TypeOptions(lang),
		"dialects":  modelcfg.DialectOptions(lang),
		"defaults": map[string]any{
			"temperature":    def.Temperature,
			"max_tokens":     def.MaxTokens,
			"thinking":       def.Thinking,
			"context_length": def.ContextLength,
		},
		"thinking_levels": []string{"off", "low", "medium", "high", "xhigh", "max"},
	}
}
