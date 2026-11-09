package server

import (
	"encoding/json"
	"net/http"

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
		Protocol string `json:"protocol"`
		Model    string `json:"model"`
		// PromptCache 未设置按开启算，与 modelcfg.Resolved 的口径一致
		PromptCache *bool `json:"prompt_cache"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "server.err.no_model_to_test", "请先为该提供商添加模型后再测试"))
		return
	}
	// 密钥留空表示沿用已保存的值（界面上只拿得到掩码）；协议同理，
	// 旧页面缓存里没有这一项时按该提供商已保存的协议测试。但接口类型被改过时
	// 不能继承：Responses 协议只属于 openai_compat，改成 Anthropic 后带回来
	// 只会在 llm.New 里变成 400。
	if p, _, ok := s.models.Lookup(req.Provider, req.Model); ok {
		if req.APIKey == "" {
			req.APIKey = p.APIKey
		}
		req.Protocol = inheritTestProtocol(req.Type, p.Type, req.Protocol, p.Protocol)
	}
	if req.APIKey == "" && modelcfg.RequiresKey(req.BaseURL) {
		writeError(w, http.StatusBadRequest, i18n.T(reqLang(r), "server.err.need_api_key", "请填写 API Key"))
		return
	}

	provider, err := llm.New(llm.Config{
		Type: req.Type, BaseURL: req.BaseURL, APIKey: req.APIKey, Dialect: req.Dialect,
		Protocol:    req.Protocol,
		PromptCache: req.PromptCache == nil || *req.PromptCache,
		Lang:        s.lang,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := llm.Probe(r.Context(), reqLang(r), provider, req.Model); err != nil {
		writeError(w, http.StatusBadRequest, i18n.Tf(reqLang(r), "server.err.probe_failed", "连接失败：%s", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// inheritTestProtocol 决定连接测试没有显式带协议时要不要沿用已保存值。
// 只在请求仍是 openai_compat、且与保存的接口类型一致时继承；请求显式给了
// 协议就照用，切换接口类型后则保持空串（等同 chat）。
func inheritTestProtocol(reqType, savedType, requested, saved string) string {
	if requested != "" {
		return requested
	}
	if reqType == llm.TypeOpenAICompat && savedType == llm.TypeOpenAICompat {
		return saved
	}
	return ""
}

// applyCurrentModel 按当前选中项与备用链重建 Provider，并热切换到 Agent。
func (s *Server) applyCurrentModel(lang string) error {
	chain, err := s.models.ResolveChain(lang)
	if err != nil {
		return err
	}
	targets := make([]agent.ModelTarget, 0, len(chain))
	for _, cur := range chain {
		provider, err := llm.New(cur.LLMConfig(s.lang))
		if err != nil {
			return err
		}
		targets = append(targets, agent.ModelTarget{
			Provider: provider,
			Options: agent.ModelOptions{
				Model:         cur.ModelID,
				Temperature:   cur.Temperature,
				MaxTokens:     cur.MaxTokens,
				Thinking:      cur.Thinking,
				ContextLength: cur.ContextLength,
			},
			Label: cur.ProviderName + " / " + cur.ModelID,
		})
	}
	s.agent.SetModelTargets(targets)
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
			"protocol":         p.Protocol,
			// 三态：true / false / null（未设置，按开启算）。界面据此区分「用户明确关过」
			// 与「从未设置」，后者保存时仍写回 null，免得一次保存就把条目从 config.yaml 接管过来。
			"prompt_cache": p.PromptCache,
			"source":       p.Source,
			"models":       models,
		})
	}
	fallbacks := view.Fallbacks
	if fallbacks == nil {
		fallbacks = []modelcfg.Selection{}
	}
	return map[string]any{
		"providers": providers,
		"current":   view.Current,
		"fallbacks": fallbacks,
		"types":     modelcfg.TypeOptions(lang),
		"dialects":  modelcfg.DialectOptions(lang),
		"protocols": modelcfg.ProtocolOptions(lang),
		// 预置随这一份下发而不是另开一个接口：它只在「添加提供商」那一步用得上，
		// 而那一步之前必然已经取过这份文档，多一次往返换不来什么。
		"presets": modelcfg.PresetOptions(lang, view.Providers),
		"defaults": map[string]any{
			"temperature":    def.Temperature,
			"max_tokens":     def.MaxTokens,
			"thinking":       def.Thinking,
			"context_length": def.ContextLength,
		},
		"thinking_levels": []string{"off", "low", "medium", "high", "xhigh", "max"},
	}
}
