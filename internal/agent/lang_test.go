package agent

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode"

	"wen/internal/i18n"
	"wen/internal/session"
)

// hanRunes 挑出一段文本里的汉字。用 unicode.Han 而不是手写码点区间：扩展区、
// 兼容表意文字都算，漏掉它们等于给漏译留了后门。
func hanRunes(s string) string {
	var out []rune
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			out = append(out, r)
		}
	}
	return string(out)
}

// TestEnglishCoreContextHasNoChinese 是英文那条路唯一直接的完备性检查。
//
// 逐条键去看译文有没有填是覆盖测试的事；这里看的是**拼出来的那份上下文**——键都填了
// 但某一处忘了走 prompt.T，覆盖测试是发现不了的，它只知道语言包里有那条键。
//
// 只管核心：插件注入的片段各归各的批次，那些由各自的测试盯。
func TestEnglishCoreContextHasNoChinese(t *testing.T) {
	dir := t.TempDir()
	store, _ := session.NewStore(dir)
	meta, _ := store.Create()
	provider := &mockProvider{turns: []mockTurn{{content: "ok"}, {content: "ok"}}}
	ag := New(provider, newTestManager(t), store, Options{Model: "test", Lang: i18n.LangEN})

	ag.Run(context.Background(), meta.ID, "hello", func(Event) {})
	ag.Run(context.Background(), meta.ID, "again", func(Event) {})

	if len(provider.reqs) == 0 {
		t.Fatal("没有发出任何请求")
	}
	for i, r := range provider.reqs {
		for j, m := range r.Messages {
			if han := hanRunes(m.Content); han != "" {
				t.Errorf("第 %d 轮第 %d 条消息里有中文 %q:\n%s", i+1, j+1, han, m.Content)
			}
		}
	}
}

// 日界线与间隔标注不经 Run 的普通路径（要隔了很久才出现），单独查一遍。
func TestEnglishTimeMarkersHaveNoChinese(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	for _, s := range []string{
		dayMark(i18n.LangEN, now),
		gapNote(i18n.LangEN, now.Add(-14*time.Hour), now),
		gapNote(i18n.LangEN, now.Add(-70*time.Minute), now),
		formatNow(i18n.LangEN, now),
		humanizeGap(i18n.LangEN, time.Minute),
		humanizeGap(i18n.LangEN, 40*24*time.Hour),
		turnStateBlock(i18n.LangEN, now, 2*time.Hour, nil),
		buildCompactPrompt(i18n.LangEN, nil, serializeHistory(i18n.LangEN, nil)),
		envContext(i18n.LangEN, "/tmp"),
	} {
		if han := hanRunes(s); han != "" {
			t.Errorf("英文输出里有中文 %q:\n%s", han, s)
		}
	}
	// 单数与复数各走一次，「about 1 hours」这种错法只在 n 为 1 时露出来
	if got := humanizeGap(i18n.LangEN, time.Minute); !strings.Contains(got, "1 minute") || strings.Contains(got, "minutes") {
		t.Errorf("一分钟应当用单数，实际 %q", got)
	}
	if got := humanizeGap(i18n.LangEN, 3*time.Hour); !strings.Contains(got, "3 hours") {
		t.Errorf("三小时应当用复数，实际 %q", got)
	}
}
