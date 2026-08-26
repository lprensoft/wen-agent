package bodysense

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func TestDecayState(t *testing.T) {
	base := time.Now()
	cases := []struct {
		name           string
		st             BodyState
		aPer, fPer     int
		after          time.Duration
		wantA, wantF   int
		wantReasonGone bool
	}{
		{"各按各的速率回落", BodyState{Arousal: 60, Fatigue: 60, Updated: base}, 30, 10, 1 * time.Hour, 30, 50, false},
		{"落到 0 不穿透", BodyState{Arousal: 20, Fatigue: 5, Updated: base}, 30, 10, 2 * time.Hour, 0, 0, false},
		{"速率 0 不回落", BodyState{Arousal: 50, Fatigue: 50, Updated: base}, 0, 0, 24 * time.Hour, 50, 50, false},
		{"时间戳缺失不猜", BodyState{Arousal: 50, Fatigue: 50}, 30, 10, 0, 50, 50, false},
		{"两项归零连起因一起丢", BodyState{Arousal: 10, Fatigue: 10, Reason: "昨晚", Updated: base}, 30, 30, 1 * time.Hour, 0, 0, true},
	}
	for _, c := range cases {
		got := decayState(c.st, c.aPer, c.fPer, base.Add(c.after))
		if got.Arousal != c.wantA || got.Fatigue != c.wantF {
			t.Errorf("%s: arousal=%d fatigue=%d, want %d/%d", c.name, got.Arousal, got.Fatigue, c.wantA, c.wantF)
		}
		if c.wantReasonGone && got.Reason != "" {
			t.Errorf("%s: reason=%q 应已清空", c.name, got.Reason)
		}
	}
	// 时钟倒流不猜
	st := BodyState{Arousal: 50, Updated: base}
	if got := decayState(st, 30, 10, base.Add(-time.Hour)); got.Arousal != 50 {
		t.Errorf("时钟倒流应原样返回，got %d", got.Arousal)
	}
}

func TestStateApplyClampsAndCaps(t *testing.T) {
	s := NewStateStore("", t.TempDir())
	now := time.Now()

	// 单次上限收拢
	st, appliedA, appliedF, err := s.Apply(80, -50, 30, 0, 0, "起因", now)
	if err != nil {
		t.Fatal(err)
	}
	if appliedA != 30 || appliedF != -30 {
		t.Errorf("applied = %d/%d, want 30/-30", appliedA, appliedF)
	}
	if st.Arousal != 30 || st.Fatigue != 0 {
		t.Errorf("state = %d/%d（疲劳不能为负）", st.Arousal, st.Fatigue)
	}

	// 量程上限
	for range 5 {
		st, _, _, err = s.Apply(30, 30, 30, 0, 0, "again", now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if st.Arousal != 100 || st.Fatigue != 100 {
		t.Errorf("state = %d/%d, want 100/100", st.Arousal, st.Fatigue)
	}

	// 两项都为 0 报错
	if _, _, _, err := s.Apply(0, 0, 30, 0, 0, "无", now); err == nil {
		t.Fatal("两个变化量都为 0 应报错")
	}
}

func TestStateApplyDecaysBeforeAdd(t *testing.T) {
	s := NewStateStore("", t.TempDir())
	start := time.Now().Add(-2 * time.Hour)
	if _, _, _, err := s.Apply(60, 0, 100, 30, 10, "先前", start); err != nil {
		t.Fatal(err)
	}
	// 两小时后 60 应已回落到 0，再 +10 应得 10 而不是 70
	st, _, _, err := s.Apply(10, 0, 100, 30, 10, "现在", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.Arousal != 10 {
		t.Errorf("arousal = %d, want 10（先折算再叠加）", st.Arousal)
	}
}

func TestVisibleStateDirectionalMerge(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()

	// 共享域记疲劳 40，里域记唤起 70
	if _, _, _, err := p.stateStoreFor("").Apply(0, 40, 100, 0, 0, "加班", now); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := p.stateStoreFor("inner").Apply(70, 0, 100, 0, 0, "亲密", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// 里人格读两个域：逐字段取最大，起因取最新那条
	inner, ok, err := p.visibleState(scoped("inner", []string{"", "inner"}))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if inner.Arousal != 70 || inner.Fatigue != 40 {
		t.Errorf("inner = %d/%d, want 70/40", inner.Arousal, inner.Fatigue)
	}
	if inner.Reason != "亲密" {
		t.Errorf("inner reason = %q", inner.Reason)
	}

	// 表人格只读共享域：里域的唤起与起因都不可见
	outer, ok, err := p.visibleState(scoped("", []string{""}))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if outer.Arousal != 0 || outer.Fatigue != 40 {
		t.Errorf("outer = %d/%d, want 0/40（不可读域不参与合并）", outer.Arousal, outer.Fatigue)
	}
	if strings.Contains(outer.Reason, "亲密") {
		t.Errorf("outer reason = %q 泄漏了里域内容", outer.Reason)
	}
}

func TestRenderBodyStateOmitsZero(t *testing.T) {
	if got := renderBodyState("", BodyState{}); got != "" {
		t.Errorf("全 0 应返回空串，got %q", got)
	}
	got := renderBodyState("", BodyState{Arousal: 50, Reason: "刚才"})
	if !strings.Contains(got, "唤起 情动（50）") || strings.Contains(got, "疲劳") {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(got, "起因：刚才") {
		t.Errorf("got %q", got)
	}
}

func TestTurnPromptIncludesBodyState(t *testing.T) {
	p := newTestPlugin(t, nil)
	ctx := scoped("", nil)
	out, err := p.Tools()[2].Execute(ctx, json.RawMessage(`{"arousal_delta":25,"reason":"靠得很近"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "唤起 微热（25，本次 +25）") {
		t.Errorf("tool out = %q", out)
	}

	got, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[当前身体状态]") || !strings.Contains(got, "唤起 微热（25）") {
		t.Errorf("TurnPrompt = %q", got)
	}
	// 没有接触记录时不该出现接触记录段
	if strings.Contains(got, "[身体接触记录]") {
		t.Errorf("TurnPrompt 不该有接触记录段：%q", got)
	}
}

func TestAdjustStateToolReportsCap(t *testing.T) {
	p := newTestPlugin(t, nil)
	out, err := p.Tools()[2].Execute(scoped("", nil), json.RawMessage(`{"arousal_delta":90,"reason":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "±30") || !strings.Contains(out, "原报 +90") {
		t.Errorf("收拢事实应回告模型：%q", out)
	}
}

func TestResetStateAction(t *testing.T) {
	p := newTestPlugin(t, nil)
	ctx := scoped("", nil)
	if _, err := p.Tools()[2].Execute(ctx, json.RawMessage(`{"fatigue_delta":30,"reason":"熬夜"}`)); err != nil {
		t.Fatal(err)
	}
	if err := p.StartAction(context.Background(), actionResetState); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := p.visibleState(ctx); ok {
		t.Error("重置后应无记录")
	}
}
