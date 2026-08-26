package sessionsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
	"wen/internal/session"
)

// visibleTitle 返回可以示人的会话标题。标题取自首条用户消息，属于生成它的那一轮
// 的可见域；不加这道判断，标题就成了绕过可见域的旁路。
func visibleTitle(lang string, meta session.Meta, sc plugin.Scope) string {
	if meta.Title == "" || !sc.CanRead(meta.Tag) {
		return prompt.T(lang, "prompt.session_search.untitled", "(无标题)")
	}
	return meta.Title
}

func errNotReady(lang string) error {
	return fmt.Errorf("%s", prompt.T(lang, "prompt.session_search.not_ready", "会话检索尚未就绪"))
}

// ---------- search_sessions ----------

type searchTool struct{ p *Plugin }

func (t *searchTool) Name() string { return "search_sessions" }

func (t *searchTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.session_search.tool.search_sessions.desc", searchDescZH)
}

const searchDescZH = "在历史对话中检索原文，可按关键词、日期范围过滤，返回命中的会话及其摘录。" +
	"用于回头查证当时具体说了什么。检索范围同时包含现存会话与被压缩前的历史归档，" +
	"归档命中后可用 read_archive 读取全文。"

func (t *searchTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.session_search.tool.search_sessions.schema", searchSchemaZH))
}

const searchSchemaZH = `{
	"type": "object",
	"properties": {
		"keyword": {"type": "string", "description": "关键词，对消息内容做包含匹配；留空则只按日期筛选"},
		"from": {"type": "string", "description": "起始日期（含），格式 YYYY-MM-DD"},
		"to": {"type": "string", "description": "结束日期（含），格式 YYYY-MM-DD"},
		"limit": {"type": "integer", "description": "最多返回多少个会话，默认 10"}
	}
}`

func (t *searchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Keyword string `json:"keyword"`
		From    string `json:"from"`
		To      string `json:"to"`
		Limit   int    `json:"limit"`
	}
	s := t.p.snapshot()
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", plugin.BadArgs(s.lang, err)
		}
	}
	if a.Limit <= 0 {
		a.Limit = 10
	}
	from, err := parseDay(s.lang, a.From, false)
	if err != nil {
		return "", err
	}
	to, err := parseDay(s.lang, a.To, true)
	if err != nil {
		return "", err
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.session_search.err.range",
			"结束日期早于起始日期"))
	}

	if s.store == nil {
		return "", errNotReady(s.lang)
	}
	cands, err := candidates(s.dir, from, to)
	if err != nil {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.session_search.err.read_dir",
			"读取会话目录失败: %w"), err)
	}
	// 现存会话为空也要继续查归档：被压缩过的内容只在归档里
	scanned, truncatedScan := cands, false
	if len(scanned) > s.maxScanSessions {
		scanned, truncatedScan = scanned[:s.maxScanSessions], true
	}

	sc := plugin.ScopeFrom(ctx)
	kw := strings.ToLower(strings.TrimSpace(a.Keyword))
	var b strings.Builder
	hits := 0
	for _, c := range scanned {
		if hits >= a.Limit {
			break
		}
		meta, msgs, err := s.store.Get(c.id)
		if err != nil {
			continue // 会话可能正好被删除，跳过而不是让整次检索失败
		}
		var matched []session.StoredMessage
		for _, m := range msgs {
			if !sc.CanRead(m.Tag) {
				continue
			}
			if !inRange(m.TS, from, to) {
				continue
			}
			if kw != "" && !strings.Contains(strings.ToLower(m.Content), kw) {
				continue
			}
			matched = append(matched, m)
		}
		if len(matched) == 0 {
			continue
		}
		hits++

		b.WriteString(prompt.Tf(s.lang, "prompt.session_search.hit.session_head",
			"## %s ｜ %s ｜ 命中 %d 条\n", visibleTitle(s.lang, meta, sc), c.id, len(matched)))
		shown := matched
		if len(shown) > s.maxSnippets {
			shown = shown[:s.maxSnippets]
		}
		for _, m := range shown {
			kind := roleLabel(s.lang, m.Role)
			if m.Kind == "summary" {
				kind = prompt.T(s.lang, "prompt.session_search.kind.summary", "压缩摘要")
			}
			fmt.Fprintf(&b, "- [%s %s] %s\n", m.TS.Format("2006-01-02 15:04"), kind, snippet(m, a.Keyword, 160))
		}
		if len(matched) > len(shown) {
			b.WriteString(prompt.Tf(s.lang, "prompt.session_search.hit.more",
				"- （另有 %d 条命中未列出，可用 read_session 读取该会话）\n", len(matched)-len(shown)))
		}
		b.WriteString("\n")
	}

	// 被压缩过的会话只剩一条摘要，其原文在归档里——这部分不查就是盲区
	archives, err := searchArchives(s.archiveDir, sc, a.Keyword, from, to, s.maxSnippets)
	if err == nil && len(archives) > 0 {
		b.WriteString(prompt.T(s.lang, "prompt.session_search.hit.archives_head", "## 压缩归档\n"))
		for _, h := range archives {
			fmt.Fprintf(&b, "- %s ｜ %s\n", h.mod.Format("2006-01-02 15:04"), h.name)
			for _, line := range h.matches {
				fmt.Fprintf(&b, "  %s\n", truncateRunes(line, 160))
			}
		}
		b.WriteString("\n")
	}

	if hits == 0 && len(archives) == 0 {
		return prompt.Tf(s.lang, "prompt.session_search.result.no_hit",
			"扫描了 %d 个会话，没有符合条件的内容。", len(scanned)), nil
	}
	head := prompt.Tf(s.lang, "prompt.session_search.result.head", "在 %d 个会话中命中 %d 个", len(scanned), hits)
	if len(archives) > 0 {
		head += prompt.Tf(s.lang, "prompt.session_search.result.head_archives",
			"，另有 %d 个压缩归档命中", len(archives))
	}
	if truncatedScan {
		head += prompt.Tf(s.lang, "prompt.session_search.result.head_capped",
			"（仅扫描了最近 %d 个会话，更早的未读取）", s.maxScanSessions)
	}
	return clip(s.lang, head+prompt.T(s.lang, "prompt.session_search.result.head_colon", "：")+
		"\n\n"+strings.TrimRight(b.String(), "\n"), s.maxBytes), nil
}

// ---------- read_archive ----------

type archiveTool struct{ p *Plugin }

func (t *archiveTool) Name() string { return "read_archive" }

func (t *archiveTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.session_search.tool.read_archive.desc", archiveDescZH)
}

const archiveDescZH = "读取一份压缩归档的内容，即某个会话在被压缩摘要替换之前的完整原文。" +
	"归档文件名取自 search_sessions 的结果；不传文件名则列出全部归档。"

func (t *archiveTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.session_search.tool.read_archive.schema", archiveSchemaZH))
}

const archiveSchemaZH = `{
	"type": "object",
	"properties": {
		"name": {"type": "string", "description": "归档文件名；留空则列出全部归档"},
		"keyword": {"type": "string", "description": "只返回包含该关键词的行；留空返回全文"}
	}
}`

func (t *archiveTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name    string `json:"name"`
		Keyword string `json:"keyword"`
	}
	s := t.p.snapshot()
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", plugin.BadArgs(s.lang, err)
		}
	}
	if s.archiveDir == "" {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.session_search.err.no_archives_on",
			"当前没有启用压缩归档"))
	}
	sc := plugin.ScopeFrom(ctx)

	if strings.TrimSpace(a.Name) == "" {
		all, err := searchArchives(s.archiveDir, sc, "", time.Time{}, time.Time{}, 0)
		if err != nil {
			return "", err
		}
		if len(all) == 0 {
			return prompt.T(s.lang, "prompt.session_search.result.no_archives", "还没有任何压缩归档。"), nil
		}
		var b strings.Builder
		b.WriteString(prompt.Tf(s.lang, "prompt.session_search.result.archive_count",
			"共 %d 份压缩归档（按时间从新到旧）：\n", len(all)))
		for _, h := range all {
			fmt.Fprintf(&b, "- %s ｜ %s\n", h.mod.Format("2006-01-02 15:04"), h.name)
		}
		return clip(s.lang, strings.TrimRight(b.String(), "\n"), s.maxBytes), nil
	}

	// 按名读取是最直接的越权入口：必须先确认这个名字既不指向目录外，
	// 也不落在本轮读不到的可见域里
	tag, name, err := parseArchiveRef(s.lang, a.Name)
	if err != nil {
		return "", err
	}
	if !sc.CanRead(tag) {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.session_search.err.no_archive",
			"没有名为 %q 的归档"), a.Name)
	}
	raw, err := os.ReadFile(filepath.Join(plugin.DomainDir(s.archiveDir, tag), name))
	if err != nil {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.session_search.err.no_archive",
			"没有名为 %q 的归档"), a.Name)
	}
	text := string(raw)

	if kw := strings.ToLower(strings.TrimSpace(a.Keyword)); kw != "" {
		var kept []string
		for line := range strings.SplitSeq(text, "\n") {
			if strings.Contains(strings.ToLower(line), kw) {
				kept = append(kept, line)
			}
		}
		if len(kept) == 0 {
			return prompt.Tf(s.lang, "prompt.session_search.result.archive_no_match",
				"归档 %s 中没有包含 %q 的内容。", name, a.Keyword), nil
		}
		text = strings.Join(kept, "\n")
	}
	return clip(s.lang, text, s.maxBytes), nil
}

// ---------- read_session ----------

type readTool struct{ p *Plugin }

func (t *readTool) Name() string { return "read_session" }

func (t *readTool) Description() string {
	return prompt.T(t.p.langOf(), "prompt.session_search.tool.read_session.desc", readDescZH)
}

const readDescZH = "读取指定会话的消息内容。会话 id 取自 search_sessions 的结果。可按日期范围或关键词收窄。"

func (t *readTool) Schema() json.RawMessage {
	return json.RawMessage(prompt.T(t.p.langOf(), "prompt.session_search.tool.read_session.schema", readSchemaZH))
}

const readSchemaZH = `{
	"type": "object",
	"properties": {
		"id": {"type": "string", "description": "会话 id"},
		"keyword": {"type": "string", "description": "只返回包含该关键词的消息；留空返回全部"},
		"from": {"type": "string", "description": "起始日期（含），格式 YYYY-MM-DD"},
		"to": {"type": "string", "description": "结束日期（含），格式 YYYY-MM-DD"}
	},
	"required": ["id"]
}`

func (t *readTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID      string `json:"id"`
		Keyword string `json:"keyword"`
		From    string `json:"from"`
		To      string `json:"to"`
	}
	s := t.p.snapshot()
	if err := json.Unmarshal(args, &a); err != nil {
		return "", plugin.BadArgs(s.lang, err)
	}
	if strings.TrimSpace(a.ID) == "" {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.session_search.err.need_id",
			"需要提供会话 id"))
	}
	from, err := parseDay(s.lang, a.From, false)
	if err != nil {
		return "", err
	}
	to, err := parseDay(s.lang, a.To, true)
	if err != nil {
		return "", err
	}

	if s.store == nil {
		return "", errNotReady(s.lang)
	}
	if _, err := os.Stat(sessionPath(s.dir, a.ID)); err != nil {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.session_search.err.no_session",
			"没有 id 为 %q 的会话"), a.ID)
	}
	meta, msgs, err := s.store.Get(a.ID)
	if err != nil {
		return "", err
	}

	// 可读条数而不是全部条数：条数本身也会泄漏「这个会话里还有别的内容」
	sc := plugin.ScopeFrom(ctx)
	readable := 0
	for _, m := range msgs {
		if sc.CanRead(m.Tag) {
			readable++
		}
	}

	kw := strings.ToLower(strings.TrimSpace(a.Keyword))
	var b strings.Builder
	b.WriteString(prompt.Tf(s.lang, "prompt.session_search.read.head",
		"# %s ｜ %s ｜ 创建于 %s ｜ 共 %d 条消息\n\n",
		visibleTitle(s.lang, meta, sc), a.ID, meta.CreatedAt.Format("2006-01-02 15:04"), readable))

	shown := 0
	for _, m := range msgs {
		if !sc.CanRead(m.Tag) {
			continue
		}
		if !inRange(m.TS, from, to) {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(m.Content), kw) {
			continue
		}
		shown++
		kind := roleLabel(s.lang, m.Role)
		if m.Kind == "summary" {
			kind = prompt.T(s.lang, "prompt.session_search.kind.summary", "压缩摘要")
		}
		fmt.Fprintf(&b, "### %s %s\n\n", m.TS.Format("2006-01-02 15:04"), kind)
		if m.Content != "" {
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		}
		for _, tc := range m.ToolCalls {
			b.WriteString(prompt.Tf(s.lang, "prompt.session_search.read.tool_call",
				"调用工具 `%s`：%s\n\n", tc.Name, truncateRunes(string(tc.Arguments), 200)))
		}
	}
	if shown == 0 {
		return prompt.Tf(s.lang, "prompt.session_search.result.session_no_match",
			"会话 %s 共 %d 条消息，没有符合条件的。", a.ID, readable), nil
	}
	return clip(s.lang, strings.TrimRight(b.String(), "\n"), s.maxBytes), nil
}
