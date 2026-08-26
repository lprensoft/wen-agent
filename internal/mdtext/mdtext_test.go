package mdtext

import (
	"strings"
	"testing"
)

func TestChunkMarkdown(t *testing.T) {
	if parts := ChunkMarkdown("短消息", 1500); len(parts) != 1 || parts[0] != "短消息" {
		t.Fatalf("短消息不该分段: %v", parts)
	}

	// 普通长文按行切且不超限
	long := strings.Repeat("这是一行比较长的内容。\n", 500)
	parts := ChunkMarkdown(long, 1500)
	if len(parts) < 3 {
		t.Fatalf("长文应分成 3 段以上，得到 %d", len(parts))
	}
	for _, part := range parts {
		if len([]rune(part)) > 1500 {
			t.Fatalf("分段超限: %d", len([]rune(part)))
		}
	}

	// 代码块被拦腰切断时：本段补闭合、下一段以原围栏重开
	code := "```go\n" + strings.Repeat("fmt.Println(1)\n", 200) + "```"
	parts = ChunkMarkdown(code, 1000)
	if len(parts) < 2 {
		t.Fatalf("长代码块应分段，得到 %d", len(parts))
	}
	for i, part := range parts {
		if i > 0 && !strings.HasPrefix(part, "```go") {
			t.Fatalf("后续段应以原围栏重开: %q", part[:20])
		}
		if !strings.HasSuffix(strings.TrimSpace(part), "```") {
			t.Fatalf("每段代码块都应闭合: …%q", part[len(part)-10:])
		}
	}

	// 超长单行硬切
	oneLine := strings.Repeat("字", 3200)
	parts = ChunkMarkdown(oneLine, 1500)
	if len(parts) != 3 {
		t.Fatalf("3200 字单行按 1500 应切成 3 段，得到 %d", len(parts))
	}
}

func TestToPlainText(t *testing.T) {
	md := strings.Join([]string{
		"# 标题",
		"**加粗** 和 *斜体* 与 `代码` 及 ~~删除~~",
		"- 列表项",
		"1. 有序项",
		"> 引用",
		"[链接](https://example.com)",
		"---",
		"```go",
		"x := \"**不要动我**\"",
		"```",
	}, "\n")
	out := ToPlainText("", md)
	for _, bad := range []string{"# ", "**加粗**", "*斜体*", "~~", "- 列表项", "> ", "](", "```"} {
		if strings.Contains(out, bad) {
			t.Fatalf("纯文本残留 %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"【标题】", "加粗", "斜体", "「代码」", "• 列表项", "1. 有序项", "｜ 引用",
		"链接（https://example.com）", `x := "**不要动我**"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("纯文本缺少 %q:\n%s", want, out)
		}
	}
}
