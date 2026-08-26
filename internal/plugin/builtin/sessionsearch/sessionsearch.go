// Package sessionsearch 提供在历史会话中按关键词与日期检索的系统插件。
//
// 与长期记忆的分工：记忆是被有意识提炼过的结论，常驻提示词；本插件面对的是
// 原始对话，信噪比低但保真，用于回头查当时到底说了什么。注意被自动压缩过的会话
// 只剩一条摘要，其原文由记忆插件的压缩归档保管。
package sessionsearch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
	"wen/internal/session"
)

const (
	defaultMaxScanSessions = 300
	defaultMaxSnippets     = 3
	defaultMaxBytes        = 8 * 1024
	defaultMaxArchives     = 20
	// idTimeLayout 对应 session.Store 生成的 id 前缀，用于免读文件判断会话创建时间。
	idTimeLayout = "20060102-150405"
)

// Plugin 是 session_search 系统插件（不注入提示词）。
type Plugin struct {
	mu sync.RWMutex

	lang            string
	dir             string
	archiveDir      string
	store           *session.Store
	maxScanSessions int
	maxSnippets     int
	maxBytes        int
	maxArchives     int
}

// langOf 取进程语言。未初始化时是空串，prompt.T 据此回中文。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

func New() *Plugin {
	return &Plugin{
		maxScanSessions: defaultMaxScanSessions,
		maxSnippets:     defaultMaxSnippets,
		maxBytes:        defaultMaxBytes,
		maxArchives:     defaultMaxArchives,
	}
}

func (p *Plugin) Name() string { return "session_search" }

func (p *Plugin) Category() string { return plugin.CategoryMemory }

func (p *Plugin) Description() string {
	return "在历史会话中按关键词与日期检索原始对话"
}

func (p *Plugin) SystemPrompt() string { return "" }

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key:         "max_scan_sessions",
			Label:       "单次最多扫描会话数",
			Type:        plugin.FieldInt,
			Description: "检索需要读取会话文件，按时间从新到旧扫描，超出数量的更早会话不再读取。",
			Default:     defaultMaxScanSessions,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(5000),
		},
		{
			Key:         "max_snippets",
			Label:       "每个会话最多返回片段数",
			Type:        plugin.FieldInt,
			Description: "同一个会话里命中多处时，最多返回几段原文。",
			Default:     defaultMaxSnippets,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(20),
		},
		{
			Key:         "max_bytes",
			Label:       "单次返回最大字节数",
			Type:        plugin.FieldInt,
			Description: "超出部分截断。",
			Default:     defaultMaxBytes,
			Min:         plugin.IntPtr(1024),
			Max:         plugin.IntPtr(256 * 1024),
		},
		{
			Key:   "max_archives",
			Label: "保留的压缩归档数",
			Type:  plugin.FieldInt,
			Description: "会话历史被压缩摘要替换之前先原样归档，检索时归档也在查找范围内；" +
				"超出数量时删除最旧的，填 0 表示不归档（被压缩过的会话将只剩摘要可查）。",
			Default: defaultMaxArchives,
			Min:     plugin.IntPtr(0),
			Max:     plugin.IntPtr(500),
		},
	}
}

// Init 需要会话目录；没有则拒绝启用。归档写在插件自己的持久化目录下。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	if ictx.SessionDir == "" {
		return fmt.Errorf("%s", i18n.T(ictx.Lang, "plugin.session_search.no_session_dir",
			"没有可用的会话目录，无法检索历史对话"))
	}
	store, err := session.NewStore(ictx.SessionDir)
	if err != nil {
		return fmt.Errorf(i18n.T(ictx.Lang, "plugin.session_search.err.open_dir",
			"打开会话目录失败: %w"), err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.lang = ictx.Lang
	p.dir = ictx.SessionDir
	p.store = store
	p.archiveDir = ""
	if ictx.StateDir != "" {
		p.archiveDir = filepath.Join(ictx.StateDir, "archives")
	}
	p.maxScanSessions = plugin.CfgInt(cfg, "max_scan_sessions", defaultMaxScanSessions)
	p.maxSnippets = plugin.CfgInt(cfg, "max_snippets", defaultMaxSnippets)
	p.maxBytes = plugin.CfgInt(cfg, "max_bytes", defaultMaxBytes)
	p.maxArchives = plugin.CfgInt(cfg, "max_archives", defaultMaxArchives)
	return nil
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{
		&searchTool{p: p},
		&readTool{p: p},
		&archiveTool{p: p},
	}
}

// settings 是一次调用期间使用的配置快照（SetConfig 会在运行时重新 Init）。
type settings struct {
	lang            string
	dir             string
	archiveDir      string
	store           *session.Store
	maxScanSessions int
	maxSnippets     int
	maxBytes        int
	maxArchives     int
}

func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:            p.lang,
		dir:             p.dir,
		archiveDir:      p.archiveDir,
		store:           p.store,
		maxScanSessions: p.maxScanSessions,
		maxSnippets:     p.maxSnippets,
		maxBytes:        p.maxBytes,
		maxArchives:     p.maxArchives,
	}
}

// candidate 是一个待检索的会话文件。
type candidate struct {
	id      string
	created time.Time // 由 id 前缀解析，失败则为零值
	touched time.Time // 文件修改时间，约等于最后一次写入时间
}

// candidates 列出会话文件并按时间从新到旧排序，同时用两个**安全**条件剪掉不可能命中的文件：
//   - 文件修改时间早于 from：追加消息会更新文件时间，故其中不存在 from 之后的消息；
//   - 会话创建时间晚于 to：消息时间不会早于会话创建时间。
//
// 反向剪枝做不到——会话可能创建于很久以前却一直在用，所以不能按创建时间剪 from，
// 这也是不能只看文件名判断日期的原因。
func candidates(dir string, from, to time.Time) ([]candidate, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []candidate
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		c := candidate{id: strings.TrimSuffix(name, ".jsonl")}
		if fi, err := de.Info(); err == nil {
			c.touched = fi.ModTime()
		}
		if len(c.id) >= len(idTimeLayout) {
			if t, err := time.ParseInLocation(idTimeLayout, c.id[:len(idTimeLayout)], time.Local); err == nil {
				c.created = t
			}
		}
		if !from.IsZero() && !c.touched.IsZero() && c.touched.Before(from) {
			continue
		}
		if !to.IsZero() && !c.created.IsZero() && c.created.After(to) {
			continue
		}
		out = append(out, c)
	}
	// id 以时间戳打头，倒序即从新到旧
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// parseDay 解析 YYYY-MM-DD；end 为 true 时取当天的最后一刻。
func parseDay(lang, s string, end bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf(prompt.T(lang, "prompt.session_search.err.bad_date",
			"日期 %q 格式不对，应为 YYYY-MM-DD"), s)
	}
	if end {
		t = t.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}
	return t, nil
}

// inRange 判断消息时间是否落在区间内（零值边界表示不限）。
func inRange(ts, from, to time.Time) bool {
	if !from.IsZero() && ts.Before(from) {
		return false
	}
	if !to.IsZero() && ts.After(to) {
		return false
	}
	return true
}

func roleLabel(lang, role string) string {
	switch role {
	case "user":
		return prompt.T(lang, "prompt.session_search.role.user", "用户")
	case "assistant":
		return prompt.T(lang, "prompt.session_search.role.assistant", "助手")
	case "tool":
		return prompt.T(lang, "prompt.session_search.role.tool", "工具结果")
	case "system":
		return prompt.T(lang, "prompt.session_search.role.system", "系统")
	}
	return role
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// snippet 把一条消息压成单行摘录，命中关键词时以命中位置为中心取上下文。
func snippet(m session.StoredMessage, keyword string, width int) string {
	text := strings.Join(strings.Fields(m.Content), " ")
	if keyword != "" {
		if i := strings.Index(strings.ToLower(text), strings.ToLower(keyword)); i >= 0 {
			runes := []rune(text)
			hit := len([]rune(text[:i]))
			start := max(0, hit-width/3)
			end := min(len(runes), start+width)
			text = string(runes[start:end])
			if start > 0 {
				text = "…" + text
			}
			if end < len(runes) {
				text += "…"
			}
			return text
		}
	}
	return truncateRunes(text, width)
}

// clip 按字节上限截断输出，并说明被截断。
func clip(lang, s string, maxBytes int) string {
	out, truncated := plugin.TruncateUTF8([]byte(s), maxBytes)
	res := string(out)
	if truncated {
		res += prompt.Tf(lang, "prompt.session_search.truncated",
			"\n...[超过 %d 字节，已截断，请缩小日期范围或用更具体的关键词]", maxBytes)
	}
	return res
}

// sessionPath 仅用于存在性检查，实际读取走 session.Store。
func sessionPath(dir, id string) string { return filepath.Join(dir, id+".jsonl") }
