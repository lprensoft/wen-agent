package textclip

import (
	"strings"
	"testing"
)

func TestClipKeepsUTF8Boundary(t *testing.T) {
	s := strings.Repeat("设", 100) // 300 字节
	got := Clip("", s, 200)
	if len(got) > 200 {
		t.Errorf("超出上限: %d 字节", len(got))
	}
	if !strings.Contains(got, "截断") {
		t.Error("截断应留下说明")
	}
	for _, r := range got {
		if r == '�' {
			t.Fatal("截断处切断了多字节字符")
		}
	}
	// 塞得下时原样返回
	if Clip("", s, 400) != s {
		t.Error("预算充足时不应截断")
	}
	if Clip("", s, 0) != "" {
		t.Error("零预算应返回空串")
	}
}

func TestClipPairFirstTakesPriority(t *testing.T) {
	a, b := strings.Repeat("甲", 100), strings.Repeat("乙", 100)
	ca, cb := ClipPair("", a, b, 400)
	if ca != a {
		t.Error("预算不足时第一段优先保全")
	}
	if len(ca)+len(cb) > 400 {
		t.Errorf("合计 %d 字节超出预算", len(ca)+len(cb))
	}
}

func TestClipSegmentsDropsWholeSegments(t *testing.T) {
	seg := strings.Repeat("样", 100) // 300 字节一段
	s := seg + "\n\n" + seg + "\n\n" + seg
	got := ClipSegments("", s, 500)
	if n := strings.Count(got, seg); n != 1 {
		t.Errorf("应整段丢弃到预算内，保留了 %d 段", n)
	}
	if !strings.Contains(got, "已略去") {
		t.Error("丢弃了段落应留下说明")
	}
	// 一段都放不下时整个不要，不做字节级截断
	if got := ClipSegments("", s, 100); got != "" {
		t.Errorf("放不下任何一段时应返回空串: %q", got)
	}
	// 塞得下时原样返回
	if ClipSegments("", s, 2000) != s {
		t.Error("预算充足时不应截断")
	}
}
