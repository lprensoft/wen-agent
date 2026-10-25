package bookshelf

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"wen/internal/plugin"
)

func newPlugin(t *testing.T, cfg map[string]any) (*Plugin, string) {
	t.Helper()
	dir := t.TempDir()
	p := New()
	now := time.Date(2026, 10, 25, 15, 0, 0, 0, time.Local)
	p.clock = func() time.Time { return now }
	if err := p.Init(plugin.InitContext{StateDir: dir}, cfg); err != nil {
		t.Fatal(err)
	}
	return p, filepath.Join(dir, "books")
}

func put(t *testing.T, shelf, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(shelf, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, tool plugin.Tool, ctx context.Context, args any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tool.Execute(ctx, raw)
}

// 一本有段落的书：每段 100 个字，共 30 段
func longBook() string {
	var paras []string
	for i := 0; i < 30; i++ {
		paras = append(paras, strings.Repeat("字", 99)+"。")
	}
	return strings.Join(paras, "\n\n")
}

func TestCutPassageBreaksAtParagraph(t *testing.T) {
	text := longBook()
	got, next := cutPassage(text, 0, 250)
	if strings.Count(got, "。") != 2 {
		t.Errorf("应退回到第 2 段结尾，得到 %d 段", strings.Count(got, "。"))
	}
	if !strings.HasSuffix(got, "。") {
		t.Error("段落应完整")
	}
	// 接着读，不重不漏
	got2, _ := cutPassage(text, next, 250)
	if utf8.RuneCountInString(got)+utf8.RuneCountInString(got2) == 0 || strings.HasPrefix(got2, "\n") {
		t.Errorf("第二段起点不对: %q", got2[:20])
	}
}

func TestCutPassageWithoutParagraphsBreaksAtSentence(t *testing.T) {
	text := strings.Repeat("他说：「走吧。」", 100) // 一整段不分行
	got, _ := cutPassage(text, 0, 50)
	if !strings.HasSuffix(got, "」") {
		t.Errorf("应断在句末并带上收尾引号: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 50 || n < 25 {
		t.Errorf("长度 %d 不在预期范围", n)
	}
}

// 硬折行的英文：单个换行不是段落结尾，应断在句末
func TestCutPassageIgnoresHardWraps(t *testing.T) {
	line := "It was the best of times, it was the worst of times, it was the age\nof wisdom, it was the age of foolishness. "
	text := strings.Repeat(line, 20)
	got, _ := cutPassage(text, 0, 300)
	if !strings.HasSuffix(got, "foolishness.") {
		t.Errorf("应断在句末而不是折行处: ...%q", got[len(got)-30:])
	}
}

func TestCutPassageHardCutsUnbrokenText(t *testing.T) {
	text := strings.Repeat("字", 1000)
	got, next := cutPassage(text, 0, 300)
	if utf8.RuneCountInString(got) != 300 || next != len("字")*300 {
		t.Errorf("没有断点时应按字数硬切: %d 字, next=%d", utf8.RuneCountInString(got), next)
	}
}

func TestReadAdvancesBookmarkAndFinishes(t *testing.T) {
	p, shelf := newPlugin(t, map[string]any{"passage_runes": 1200})
	put(t, shelf, "呼兰河传.txt", longBook())
	read := &readTool{p: p}
	ctx := context.Background()

	out, err := call(t, read, ctx, map[string]any{"book": "呼兰河"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "《呼兰河传》") || !strings.Contains(out, "读到约") {
		t.Errorf("结果缺书名或进度: %.80s", out)
	}
	// 不写书名接着读手上那本，直到读完
	finished := false
	for i := 0; i < 10 && !finished; i++ {
		out, err = call(t, read, ctx, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		finished = strings.Contains(out, "全书读完了")
	}
	if !finished {
		t.Fatal("读不完")
	}
	out, _ = call(t, read, ctx, map[string]any{})
	if !strings.Contains(out, "已经读完了") {
		t.Errorf("读完后再读应提示已读完: %s", out)
	}
	out, _ = call(t, read, ctx, map[string]any{"from_start": true})
	if !strings.Contains(out, "读到约") {
		t.Errorf("from_start 应从头再读: %.60s", out)
	}
}

// 门禁在代码里：真人在场的轮次不翻开新的一段，印象照记
func TestInteractiveTurnCannotOpenNewPassage(t *testing.T) {
	p, shelf := newPlugin(t, nil)
	put(t, shelf, "边城.txt", longBook())
	live := plugin.WithInteractive(context.Background())
	if _, err := call(t, &readTool{p: p}, live, map[string]any{"book": "边城"}); err == nil ||
		!strings.Contains(err.Error(), "跟前") {
		t.Fatalf("真人在场时应拒绝读新的一段: %v", err)
	}
	// 后台读过一段之后，聊天时可以记印象
	if _, err := call(t, &readTool{p: p}, context.Background(), map[string]any{"book": "边城"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, &noteTool{p: p}, live, map[string]any{"note": "渡口写得真静"}); err != nil {
		t.Fatalf("聊天时应能记印象: %v", err)
	}
}

func TestNoteRequiresReadingAndIsCapped(t *testing.T) {
	p, shelf := newPlugin(t, nil)
	put(t, shelf, "小王子.md", longBook())
	note := &noteTool{p: p}
	ctx := context.Background()
	if _, err := call(t, note, ctx, map[string]any{"book": "小王子", "note": "玫瑰"}); err == nil {
		t.Error("没读过的书不该有印象")
	}
	if _, err := call(t, &readTool{p: p}, ctx, map[string]any{"book": "小王子"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, note, ctx, map[string]any{"note": strings.Repeat("长", maxNoteRunes+1)}); err == nil {
		t.Error("超长的印象应报错而不是截断")
	}
	for i := 0; i < maxNotes+2; i++ {
		if _, err := call(t, note, ctx, map[string]any{"note": "第" + string(rune('A'+i)) + "条"}); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := p.storeFor("").Load()
	notes := st.Books["小王子.md"].Notes
	if len(notes) != maxNotes || notes[0].Text != "第C条" {
		t.Errorf("应只留最近 %d 条: %+v", maxNotes, notes)
	}
}

func TestBookNameMatching(t *testing.T) {
	p, shelf := newPlugin(t, nil)
	put(t, shelf, "三体1.txt", "甲。")
	put(t, shelf, "三体2.txt", "乙。")
	put(t, shelf, "不是书.pdf", "x")
	read := &readTool{p: p}
	ctx := context.Background()
	if _, err := call(t, read, ctx, map[string]any{"book": "三体"}); err == nil || !strings.Contains(err.Error(), "好几本") {
		t.Errorf("两本都对得上时应要求说全名: %v", err)
	}
	if out, err := call(t, read, ctx, map[string]any{"book": "《三体2》"}); err != nil || !strings.Contains(out, "乙") {
		t.Errorf("书名号应能去掉: %v %s", err, out)
	}
	if _, err := call(t, read, ctx, map[string]any{"book": "不是书"}); err == nil {
		t.Error("只认 .txt 与 .md")
	}
	if _, err := call(t, read, ctx, map[string]any{}); err != nil {
		t.Errorf("不写书名应接着读手上那本: %v", err)
	}
}

func TestDecodesGBKAndUTF16(t *testing.T) {
	gbk, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("春眠不觉晓。"))
	if got := decodeText(gbk); got != "春眠不觉晓。" {
		t.Errorf("GBK 解码不对: %q", got)
	}
	u16 := []byte{0xFF, 0xFE}
	for _, r := range "处处闻啼鸟。\r\n" {
		u16 = append(u16, byte(r), byte(r>>8))
	}
	if got := decodeText(u16); got != "处处闻啼鸟。\n" {
		t.Errorf("UTF-16 解码不对: %q", got)
	}
	if got := decodeText([]byte("\xef\xbb\xbf夜来风雨声")); got != "夜来风雨声" {
		t.Errorf("应去掉 BOM: %q", got)
	}
}

// 进度属于人格：一面读过的另一面不知道
func TestProgressIsPerScope(t *testing.T) {
	p, shelf := newPlugin(t, nil)
	put(t, shelf, "边城.txt", longBook())
	inner := plugin.WithScope(context.Background(), plugin.Scope{Write: "inner"})
	if _, err := call(t, &readTool{p: p}, inner, map[string]any{"book": "边城"}); err != nil {
		t.Fatal(err)
	}
	outer, _ := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if strings.Contains(outer, "在读：") {
		t.Errorf("表面不该知道里面读过: %s", outer)
	}
	in, _ := p.TurnPrompt(inner, plugin.TurnEvent{})
	if !strings.Contains(in, "在读：《边城》") {
		t.Errorf("里面应看到自己的进度: %s", in)
	}
}

func TestTurnPrompt(t *testing.T) {
	p, shelf := newPlugin(t, nil)
	ctx := context.Background()
	if got, _ := p.TurnPrompt(ctx, plugin.TurnEvent{}); got != "" {
		t.Errorf("书架空着时不注入: %q", got)
	}
	put(t, shelf, "边城.txt", longBook())
	put(t, shelf, "呼兰河传.txt", longBook())
	if _, err := call(t, &readTool{p: p}, ctx, map[string]any{"book": "边城"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, &noteTool{p: p}, ctx, map[string]any{"note": "翠翠在等"}); err != nil {
		t.Fatal(err)
	}
	got, _ := p.TurnPrompt(ctx, plugin.TurnEvent{})
	for _, want := range []string{"[在读的书]", "书架上有：《呼兰河传》、《边城》。", "在读：《边城》读到约", "上次读是今天", "「翠翠在等」"} {
		if !strings.Contains(got, want) {
			t.Errorf("注入缺 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "没人在跟前") {
		t.Error("非心跳轮次不该有「可以翻一段」")
	}
	hb := plugin.WithTurnOrigin(ctx, "heartbeat")
	if got, _ := p.TurnPrompt(hb, plugin.TurnEvent{}); !strings.Contains(got, "没人在跟前") {
		t.Errorf("心跳轮次应提一句可以读: %s", got)
	}
}

func TestShelfAction(t *testing.T) {
	p, shelf := newPlugin(t, nil)
	if err := p.StartAction(context.Background(), actionShelf); err != nil {
		t.Fatal(err)
	}
	st, _ := p.ActionState(actionShelf)
	if st.Status != plugin.ActionDone || !strings.Contains(st.Message, "还没有书") {
		t.Errorf("空书架: %+v", st)
	}
	put(t, shelf, "边城.txt", "翠翠。")
	_ = p.StartAction(context.Background(), actionShelf)
	st, _ = p.ActionState(actionShelf)
	if !strings.Contains(st.Message, "边城：约 3 字，还没读") {
		t.Errorf("报表不对: %s", st.Message)
	}
}
