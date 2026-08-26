package sessionsearch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

// OnCompact 在会话历史被摘要替换之前把原始历史原样归档。
//
// 自动压缩是无人值守触发的，且会物理删除整段历史；归档是这些原文唯一的挽回途径，
// 也让 search_sessions 能覆盖已经被压缩过的会话——否则那些会话只剩一条摘要，
// 恰好是最值得回查的部分成了盲区。
//
// 这里只做文件写入，不发起模型调用；从历史中提炼长期记忆是 memory 插件订阅同一个
// 钩子完成的事。
// 归档按可见域分目录：ev.Scope 决定这一组历史落在哪个目录下。共享域用基准目录
// （升级前的归档因此照旧可读），其余域用同级的 <archives>-<tag>。
func (p *Plugin) OnCompact(_ context.Context, ev plugin.CompactEvent) (string, error) {
	s := p.snapshot()
	if s.archiveDir == "" || s.maxArchives <= 0 || len(ev.History) == 0 {
		return "", nil
	}
	dir := plugin.DomainDir(s.archiveDir, ev.Scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.session_search.err.mkdir_archive",
			"创建归档目录失败: %w"), err)
	}

	name := fmt.Sprintf("%s-%s.md", sanitizeID(ev.SessionID), time.Now().Format("20060102-150405"))
	path := filepath.Join(dir, name)
	raw := []byte(renderArchive(s.lang, ev))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.session_search.err.write_archive",
			"写入归档失败: %w"), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf(prompt.T(s.lang, "prompt.session_search.err.write_archive",
			"写入归档失败: %w"), err)
	}
	// 按域各留 N 份：全局计数的话，写得频繁的那个域会把别的域的归档挤掉，
	// 那不是泄漏而是数据丢失
	pruneArchives(dir, s.maxArchives)

	return prompt.Tf(s.lang, "prompt.session_search.compact.note",
		"（本次压缩前的完整历史已归档：%s）", archiveRef(ev.Scope, name)), nil
}

// archiveRef / parseArchiveRef 是归档在工具接口上的标识：带域的归档写成 <tag>/<name>，
// 共享域的仍是裸文件名。模型看到什么就能读什么，无需理解目录布局。
func archiveRef(tag, name string) string {
	if tag == "" {
		return name
	}
	return tag + "/" + name
}

// parseArchiveRef 拆出标识里的域与文件名，并确认二者都不指向目录外。
func parseArchiveRef(lang, ref string) (tag, name string, err error) {
	ref = strings.TrimSpace(strings.ReplaceAll(ref, "\\", "/"))
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		tag, name = ref[:i], ref[i+1:]
	} else {
		name = ref
	}
	name = filepath.Base(name)
	if name == "" || name == "." {
		return "", "", fmt.Errorf(prompt.T(lang, "prompt.session_search.err.bad_ref",
			"归档名 %q 不合法"), ref)
	}
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	if tag != "" && !validDomain(tag) {
		return "", "", fmt.Errorf(prompt.T(lang, "prompt.session_search.err.bad_ref",
			"归档名 %q 不合法"), ref)
	}
	return tag, name, nil
}

// validDomain 与插件名同规。可见域标签会被拼进目录，不能放过任意字符串。
func validDomain(tag string) bool {
	if tag == "" {
		return true
	}
	for i, r := range tag {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_'):
		default:
			return false
		}
	}
	return true
}

// sanitizeID 只保留字母数字与连字符下划线，避免异常的会话 id 影响归档文件名。
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "session"
	}
	return out
}

// renderArchive 把历史渲染成可读的 Markdown。与压缩用的序列化不同，
// 这里不做任何截断——归档的意义就在于保住原文。
func renderArchive(lang string, ev plugin.CompactEvent) string {
	var b strings.Builder
	b.WriteString(prompt.T(lang, "prompt.session_search.archive.title", "# 压缩归档\n\n"))
	b.WriteString(prompt.Tf(lang, "prompt.session_search.archive.session", "- 会话：%s\n", ev.SessionID))
	b.WriteString(prompt.Tf(lang, "prompt.session_search.archive.at", "- 归档时间：%s\n",
		time.Now().Format("2006-01-02 15:04:05")))
	b.WriteString(prompt.Tf(lang, "prompt.session_search.archive.count", "- 消息数：%d\n\n", len(ev.History)))

	b.WriteString(prompt.T(lang, "prompt.session_search.archive.summary_head", "## 摘要\n\n"))
	b.WriteString(ev.Summary)
	b.WriteString(prompt.T(lang, "prompt.session_search.archive.history_head", "\n\n## 原始历史\n\n"))

	for _, m := range ev.History {
		switch m.Role {
		case llm.RoleUser:
			b.WriteString(prompt.T(lang, "prompt.session_search.archive.user", "### 用户\n\n"))
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		case llm.RoleAssistant:
			b.WriteString(prompt.T(lang, "prompt.session_search.archive.assistant", "### 助手\n\n"))
			if m.Reasoning != "" {
				b.WriteString(prompt.T(lang, "prompt.session_search.archive.think_open", "<思考>\n"))
				b.WriteString(m.Reasoning)
				b.WriteString(prompt.T(lang, "prompt.session_search.archive.think_close", "\n</思考>\n\n"))
			}
			if m.Content != "" {
				b.WriteString(m.Content)
				b.WriteString("\n\n")
			}
			for _, tc := range m.ToolCalls {
				b.WriteString(prompt.Tf(lang, "prompt.session_search.archive.tool_call",
					"调用工具 `%s`：\n\n```json\n%s\n```\n\n", tc.Name, string(tc.Arguments)))
			}
		case llm.RoleTool:
			b.WriteString(prompt.T(lang, "prompt.session_search.archive.tool_result", "### 工具结果\n\n```\n"))
			b.WriteString(m.Content)
			b.WriteString("\n```\n\n")
		case llm.RoleSystem:
			b.WriteString(prompt.T(lang, "prompt.session_search.archive.system", "### 系统\n\n"))
			b.WriteString(m.Content)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// pruneArchives 只保留最近的 max 个归档。
// 按修改时间而不是文件名排序：文件名以会话 id 打头，字典序会先按会话分组，不是时间序。
func pruneArchives(dir string, max int) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type item struct {
		name string
		mod  time.Time
	}
	var items []item
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".md") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		items = append(items, item{de.Name(), fi.ModTime()})
	}
	if len(items) <= max {
		return
	}
	slices.SortFunc(items, func(a, b item) int { return a.mod.Compare(b.mod) })
	for _, it := range items[:len(items)-max] {
		_ = os.Remove(filepath.Join(dir, it.name))
	}
}

// archiveHit 是归档文件中的一处命中。
type archiveHit struct {
	name    string // 对外标识，见 archiveRef
	mod     time.Time
	matches []string
}

// searchArchives 在本轮可读的各个归档目录里做包含匹配，返回命中的文件与摘录。
func searchArchives(base string, sc plugin.Scope, keyword string, from, to time.Time, maxSnippets int) ([]archiveHit, error) {
	if base == "" {
		return nil, nil
	}
	var hits []archiveHit
	for _, tag := range plugin.ReadDomains(base, sc) {
		found, err := searchArchiveDir(plugin.DomainDir(base, tag), tag, keyword, from, to, maxSnippets)
		if err != nil {
			continue // 某个域读不出来不该让其余域也查不了
		}
		hits = append(hits, found...)
	}
	slices.SortFunc(hits, func(a, b archiveHit) int { return b.mod.Compare(a.mod) })
	return hits, nil
}

func searchArchiveDir(dir, tag, keyword string, from, to time.Time, maxSnippets int) ([]archiveHit, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	kw := strings.ToLower(strings.TrimSpace(keyword))

	var hits []archiveHit
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".md") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		// 归档写入时间即其内容的时间下界，可据此按日期筛选
		if !inRange(fi.ModTime(), from, to) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			continue
		}
		text := string(raw)
		if kw != "" && !strings.Contains(strings.ToLower(text), kw) {
			continue
		}
		hit := archiveHit{name: archiveRef(tag, de.Name()), mod: fi.ModTime()}
		if kw != "" {
			for line := range strings.SplitSeq(text, "\n") {
				if strings.Contains(strings.ToLower(line), kw) {
					hit.matches = append(hit.matches, strings.TrimSpace(line))
					if maxSnippets > 0 && len(hit.matches) >= maxSnippets {
						break
					}
				}
			}
		}
		hits = append(hits, hit)
	}
	return hits, nil
}
