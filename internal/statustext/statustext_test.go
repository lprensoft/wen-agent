package statustext

import (
	"strings"
	"testing"

	"wen/internal/plugin"
)

func base() plugin.StatusInfo {
	return plugin.StatusInfo{
		Version: "v0.1.3", Provider: "deepseek", Model: "deepseek-v4-flash",
		Thinking: "high", ContextLength: 1000000, MeasuredTokens: -1,
	}
}

func TestComma(t *testing.T) {
	cases := map[int]string{131072: "131,072", 999: "999", 0: "0", 1000000: "1,000,000", -4567: "-4,567"}
	for in, want := range cases {
		if got := Comma(in); got != want {
			t.Errorf("Comma(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestPct(t *testing.T) {
	if got := Pct(1234, 131072); got != "0.9" {
		t.Errorf("Pct = %s, want 0.9", got)
	}
	if got := Pct(1, 0); got != "0.0" {
		t.Errorf("除零 = %s, want 0.0", got)
	}
}

// 模型与思考深度同一行，上下文窗口并进会话那一行。
func TestRenderMergesLines(t *testing.T) {
	info := base()
	info.HasSession = true
	info.MessageCount = 12
	info.MeasuredTokens = 45231

	got := Render("", info, "sess-1")
	lines := strings.Split(got, "\n")

	if lines[0] != "📊 Wen Agent v0.1.3" {
		t.Errorf("首行 = %q", lines[0])
	}
	if lines[1] != "模型：deepseek / deepseek-v4-flash · 思考深度 high" {
		t.Errorf("模型行 = %q", lines[1])
	}
	if lines[2] != "当前会话：12 条消息，45,231 / 1,000,000 tokens（占用 4.5%）" {
		t.Errorf("会话行 = %q", lines[2])
	}
	if lines[3] != "会话 ID：sess-1" {
		t.Errorf("会话 ID 行 = %q", lines[3])
	}
	if len(lines) != 4 {
		t.Errorf("共 %d 行，超出预期的 4 行：\n%s", len(lines), got)
	}
	// 上下文窗口不再单独占一行
	for _, l := range lines {
		if strings.HasPrefix(l, "上下文窗口") {
			t.Errorf("上下文窗口仍单独成行: %q", l)
		}
	}
}

// 没有实测值时用估算值，靠一个「约」字区分，不再另写「（估算）」。
func TestRenderEstimated(t *testing.T) {
	info := base()
	info.HasSession = true
	info.MessageCount = 3
	info.MeasuredTokens = -1
	info.EstTokens = 2048

	line := strings.Split(Render("", info, ""), "\n")[2]
	if line != "当前会话：3 条消息，约 2,048 / 1,000,000 tokens（占用 0.2%）" {
		t.Errorf("估算行 = %q", line)
	}
	if strings.Contains(line, "估算") {
		t.Errorf("「约」已经表达了估算，不必再写一次: %q", line)
	}
}

// 无会话时仍要能看到上下文窗口——它是模型配置的一部分，不随会话消失。
func TestRenderWithoutSession(t *testing.T) {
	got := Render("", base(), "")
	if !strings.Contains(got, "当前会话：无（上下文窗口 1,000,000 tokens）") {
		t.Errorf("无会话时的输出 = %q", got)
	}
	if strings.Contains(got, "会话 ID") {
		t.Error("无会话时不该出现会话 ID")
	}
}

func TestCacheLine(t *testing.T) {
	info := base()
	if CacheLine("", info) != "" {
		t.Error("没用上缓存时应返回空串")
	}
	info.CachedTokens = 30000
	info.CacheWriteTokens = 5000
	info.PromptTokens = 45000
	want := "提示词缓存：命中 30,000 / 写入 5,000 tokens（占本轮输入 66.7%）"
	if got := CacheLine("", info); got != want {
		t.Errorf("缓存行 = %q\nwant %q", got, want)
	}
}

// 插件状态行接在会话行之后，按传入顺序原样输出，核心不解释其内容。
func TestRenderAppendsPluginLines(t *testing.T) {
	info := base()
	info.PluginLines = []string{"💓 心跳：…", "🧠 记忆：…"}
	lines := strings.Split(Render("", info, ""), "\n")
	if lines[len(lines)-2] != "💓 心跳：…" || lines[len(lines)-1] != "🧠 记忆：…" {
		t.Errorf("插件行未按序附在末尾:\n%s", strings.Join(lines, "\n"))
	}
}
