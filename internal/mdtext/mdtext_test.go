package mdtext

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestChunkMarkdownBytes(t *testing.T) {
	// 按字节切：汉字 3 字节、emoji 4 字节，每段都不能超过上限
	text := strings.Repeat("汉字🙂", 900)
	parts := ChunkMarkdownBytes(text, 1000)
	if len(parts) < 2 {
		t.Fatalf("按 1000 字节应分成多段，得到 %d", len(parts))
	}
	var joined strings.Builder
	for _, part := range parts {
		if len(part) > 1000 {
			t.Fatalf("分段超字节上限: %d", len(part))
		}
		if !utf8.ValidString(part) {
			t.Fatalf("切出了半个 rune: %q", part)
		}
		joined.WriteString(part)
	}
	if joined.String() != text {
		t.Fatal("按字节分段后内容不一致")
	}

	// 代码块按字节断开时同样要闭合与重开
	code := "```go\n" + strings.Repeat("var 变量 = 1\n", 200) + "```"
	parts = ChunkMarkdownBytes(code, 800)
	if len(parts) < 2 {
		t.Fatalf("长代码块应分段，得到 %d", len(parts))
	}
	for i, part := range parts {
		if len(part) > 800 {
			t.Fatalf("代码段超字节上限: %d", len(part))
		}
		if i > 0 && !strings.HasPrefix(part, "```go") {
			t.Fatalf("后续段应以原围栏重开: %q", part[:10])
		}
		if !strings.HasSuffix(strings.TrimSpace(part), "```") {
			t.Fatalf("每段代码块都应闭合: …%q", part[len(part)-10:])
		}
	}

	// 超长单行按字节硬切，且不切断 rune
	oneLine := strings.Repeat("字", 400)
	parts = ChunkMarkdownBytes(oneLine, 1000)
	if len(parts) != 2 || len(parts[0]) > 1000 || len(parts[1]) > 1000 {
		t.Fatalf("400 个汉字按 1000 字节应切成两段: %v", len(parts))
	}
	if parts[0]+parts[1] != oneLine {
		t.Fatal("硬切后内容不一致")
	}
}

func TestChunkMarkdownClosesCodeBlockOnLongLine(t *testing.T) {
	// 代码块里的超长单行被硬切时，本段也要补闭合围栏，不能以开头围栏结尾
	code := "```go\n" + strings.Repeat("x", 3000) + "\n```"
	parts := ChunkMarkdown(code, 1000)
	for _, part := range parts {
		if strings.HasPrefix(part, "```go") && !strings.HasSuffix(strings.TrimSpace(part), "```") {
			t.Fatalf("代码段未闭合: …%q", part[len(part)-10:])
		}
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
