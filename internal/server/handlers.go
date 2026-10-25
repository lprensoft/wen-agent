package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"wen/internal/agent"
	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/session"
	"wen/internal/version"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) listSessions(w http.ResponseWriter, _ *http.Request) {
	metas, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, metas)
}

func (s *Server) createSession(w http.ResponseWriter, _ *http.Request) {
	meta, err := s.store.Create()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, meta)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	meta, msgs, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	if msgs == nil {
		msgs = []session.StoredMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": meta, "messages": msgs})
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// chat 处理一条用户消息，以 SSE 流返回 Agent 事件。
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
		// DebugPrompt 打开时逐次发回实际提交给模型的请求体，供界面调试查看。
		// 默认关闭：整轮的完整上下文可能有数 MB，不看的时候不该付这份代价。
		DebugPrompt bool `json:"debug_prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.SessionID == "" || req.Message == "" {
		writeError(w, http.StatusBadRequest, "session_id and message are required")
		return
	}
	if _, _, err := s.store.Get(req.SessionID); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session not found: %s", req.SessionID))
		return
	}

	emit, emitRaw, ok := startSSE(w)
	if !ok {
		return
	}
	// 工具要请求用户确认时经这条 SSE 流问、经 /api/confirmations/{id} 收答复。
	// 按请求注入而不是放进 InitContext：确认必须回到发起这轮对话的那个界面。
	ctx := plugin.WithConfirmer(r.Context(), s.confirmerFor(emitRaw))
	// 前台界面的轮次有真人在交互：更新会话活跃时间，供后台功能判定「最近活跃会话」
	ctx = plugin.WithInteractive(ctx)
	if req.DebugPrompt {
		ctx = agent.WithPromptTrace(ctx)
	}
	s.agent.Run(ctx, req.SessionID, req.Message, emit)
}

// startSSE 设置 SSE 响应头，返回 Agent 事件写入函数与一个原始帧写入函数
// （Agent 与工具执行同在一个 goroutine 上，无需加锁）。
// 原始帧留给不属于 Agent 事件模型的东西，如工具的确认请求。
func startSSE(w http.ResponseWriter) (func(agent.Event), func(string, any), bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emitRaw := func(name string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		flusher.Flush()
	}
	emit := func(ev agent.Event) {
		emitRaw(string(ev.Type), ev)
	}
	return emit, emitRaw, true
}

// status 返回 Agent 配置与（可选的）当前会话上下文用量。
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	provider, model, thinking, contextLength := s.models.Status()
	resp := map[string]any{
		"version":        version.Version,
		"provider":       provider,
		"model":          model,
		"thinking":       thinking,
		"context_length": contextLength,
	}
	// 插件贡献的状态行（如心跳节奏）：无插件可报时不下发该字段，界面据此不占版面
	if lines := s.plugins.StatusLines(reqLang(r)); len(lines) > 0 {
		resp["plugin_lines"] = lines
	}
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		if meta, msgs, err := s.store.Get(sid); err == nil {
			sess := map[string]any{
				"id":            sid,
				"message_count": len(msgs),
				"est_tokens":    agent.EstimateStoredTokens(msgs),
			}
			if meta.LastUsage != nil {
				// 实测值：最近一次请求的完整上下文 + 本次输出
				sess["measured_tokens"] = meta.LastUsage.PromptTokens + meta.LastUsage.CompletionTokens
				// 缓存命中：只在真的命中或写入过时下发，界面据此不占版面
				if meta.LastUsage.CachedTokens > 0 || meta.LastUsage.CacheWriteTokens > 0 {
					sess["cached_tokens"] = meta.LastUsage.CachedTokens
					sess["cache_write_tokens"] = meta.LastUsage.CacheWriteTokens
					sess["prompt_tokens"] = meta.LastUsage.PromptTokens
				}
			}
			resp["session"] = sess
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// reqLang 判定这一次请求要用哪种语言展示插件元数据。
//
// 按请求判定而不是存在服务端：同一个服务可能同时连着一个中文浏览器和一个英文
// 的 wen config，服务端记住「当前语言」就等于让后到的人改掉先到的人的界面。
// 显式的 ?lang= 优先于 Accept-Language——前者是用户在界面上选的，后者只是
// 浏览器的默认偏好，用户选了中文界面却装着英文浏览器是完全正常的组合。
func reqLang(r *http.Request) string {
	if v := r.URL.Query().Get("lang"); v != "" {
		if i18n.Supported(v) {
			return v
		}
	}
	if h := r.Header.Get("Accept-Language"); h != "" {
		if lang := i18n.Match(acceptLanguages(h)...); lang != "" {
			return lang
		}
	}
	return i18n.Default
}

// acceptLanguages 把 Accept-Language 拆成按 q 值排好的语言标识。
// 只做到「按 q 降序」为止，不实现完整的 RFC 4647 匹配——这里要挑的是两三种
// 语言里的一种，把标准实现全了也不会挑得更准。
func acceptLanguages(h string) []string {
	type item struct {
		tag string
		q   float64
	}
	var items []item
	for _, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, q := part, 1.0
		if i := strings.Index(part, ";"); i >= 0 {
			tag = strings.TrimSpace(part[:i])
			if v := strings.TrimSpace(part[i+1:]); strings.HasPrefix(v, "q=") {
				if f, err := strconv.ParseFloat(v[2:], 64); err == nil {
					q = f
				}
			}
		}
		items = append(items, item{tag, q})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].q > items[j].q })
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.tag)
	}
	return out
}

// listPlugins 返回全部插件状态。
func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, plugin.Localize(reqLang(r), s.plugins.List()))
}

// setPlugin 运行时启用/禁用插件。
func (s *Server) setPlugin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.plugins.SetEnabled(r.PathValue("name"), req.Enabled); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plugin.Localize(reqLang(r), s.plugins.List()))
}

// setPluginConfig 保存插件配置，保存成功后立即生效。
func (s *Server) setPluginConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.plugins.SetConfig(r.PathValue("name"), req.Config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plugin.Localize(reqLang(r), s.plugins.List()))
}

// startPluginAction 触发插件的一个操作入口（长流程在插件后台进行，本请求立即返回）。
//
// 请求体可选，形如 {"config": {...}, "token": "..."}：config 是配置弹窗里当前填写、尚未
// 保存的值，随 ctx 交给插件（见 plugin.WithActionValues），使「测试」类操作能验证还没保存
// 的配置；token 是按钮展示时插件给的凭据（见 plugin.ActionDef.Token），原样交回，插件据此
// 核对用户点的是哪一个。空 body 合法，解析失败也只当作两样都没带——操作本身不该因为这个
// 失败，需要凭据的插件自己会拒绝。
func (s *Server) startPluginAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Config map[string]any `json:"config"`
		Token  string         `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ctx := plugin.WithActionValues(r.Context(), req.Config)
	ctx = plugin.WithActionToken(ctx, req.Token)
	if err := s.plugins.StartAction(ctx, r.PathValue("name"), r.PathValue("key")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

// pluginActionState 查询插件操作的进展，供界面轮询。
func (s *Server) pluginActionState(w http.ResponseWriter, r *http.Request) {
	st, err := s.plugins.ActionState(r.PathValue("name"), r.PathValue("key"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// compact 压缩指定会话，过程以 SSE 流返回。
func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, err := s.store.Get(id); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session not found: %s", id))
		return
	}
	emit, _, ok := startSSE(w)
	if !ok {
		return
	}
	s.agent.Compact(r.Context(), id, emit)
}
