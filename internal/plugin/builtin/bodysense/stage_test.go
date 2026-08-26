package bodysense

import (
	"testing"

	"wen/internal/prompt"
)

func TestStageBoundaries(t *testing.T) {
	cases := []struct {
		count int
		want  prompt.Piece
	}{
		{0, prompt.Piece{}},
		{1, stageFirst},
		{2, stageStrange},
		{3, stageStrange},
		{4, stageAdapting},
		{9, stageAdapting},
		{10, stageFamiliar},
		{19, stageFamiliar},
		{20, stageUsual},
		{200, stageUsual},
	}
	for _, c := range cases {
		if got := stageOf(c.count, paceNormal); got != c.want {
			t.Errorf("stageOf(%d) = %q，期望 %q", c.count, got.ZH(), c.want.ZH())
		}
	}
}

func TestStagePace(t *testing.T) {
	// 快档次数加倍：10 次就到「习以为常」
	if got := stageOf(10, paceFast); got != stageUsual {
		t.Errorf("快档 10 次 = %q，期望 %q", got.ZH(), stageUsual.ZH())
	}
	// 慢档次数减半：20 次只到「熟悉」
	if got := stageOf(20, paceSlow); got != stageFamiliar {
		t.Errorf("慢档 20 次 = %q，期望 %q", got.ZH(), stageFamiliar.ZH())
	}
	// 慢档下第一次仍要算「初次」，不能因为取整塌成 0
	if got := stageOf(1, paceSlow); got != stageFirst {
		t.Errorf("慢档 1 次 = %q，期望 %q", got.ZH(), stageFirst.ZH())
	}
	// 零次在任何档位下都没有阶段
	for _, pace := range []string{paceSlow, paceNormal, paceFast} {
		if got := stageOf(0, pace); got.Key() != "" {
			t.Errorf("%s 档 0 次 = %q，期望零值", pace, got.ZH())
		}
	}
}
