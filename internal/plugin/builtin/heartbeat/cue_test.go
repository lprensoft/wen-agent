package heartbeat

import (
	"context"
	"strings"
	"testing"
	"time"

	"wen/internal/cue"
	"wen/internal/plugin"
)

func drainCues() { cue.Take(time.Now().Add(24 * time.Hour)) }

func postCue(text string) {
	cue.Post(cue.Cue{Source: "test", Key: text, Text: text, Expire: time.Now().Add(time.Hour)})
}

// 有待说的理由时下一拍提前到最快间隔；固定节奏不提前；暂停压过一切。
func TestNextBeatPulledForwardByCue(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})

	p.mu.Lock()
	p.lastBeat = time.Now()
	base := p.nextBeatLocked()
	p.mu.Unlock()

	postCue("下雨了")
	p.mu.Lock()
	pulled := p.nextBeatLocked()
	p.mu.Unlock()
	if !pulled.Before(base) {
		t.Errorf("有理由待说时下一拍应提前: base=%v pulled=%v", base, pulled)
	}
	if got := pulled.Sub(p.lastBeat); got != 5*time.Minute {
		t.Errorf("应提前到最快间隔，得到 %v", got)
	}

	// 暂停仍然压过提前：睡着的人不为一场雨叫醒
	p.mu.Lock()
	p.pausedUntil = time.Now().Add(8 * time.Hour)
	paused := p.nextBeatLocked()
	p.pausedUntil = time.Time{}
	p.mu.Unlock()
	if paused.Sub(p.lastBeat) < 8*time.Hour {
		t.Errorf("暂停期间不该被理由叫醒: %v", paused)
	}
	drainCues()

	// 固定节奏雷打不动
	q, _ := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120, "dynamic": false,
	})
	postCue("下雨了")
	q.mu.Lock()
	q.lastBeat = time.Now()
	fixed := q.nextBeatLocked()
	q.mu.Unlock()
	if got := fixed.Sub(q.lastBeat); got != 60*time.Minute {
		t.Errorf("固定节奏不该提前: %v", got)
	}
}

func turnCueCtx(write string, read []string) context.Context {
	ctx := plugin.WithTurnOrigin(context.Background(), "heartbeat")
	return plugin.WithScope(ctx, plugin.Scope{Write: write, Read: read})
}

// 别的人格域的理由留在队列里，但不能把这一人格的心跳反复提前叫醒。
func TestNextBeatIgnoresOtherDomainCuesForPull(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, map[string]any{
		"interval_minutes": 60, "min_minutes": 5, "max_minutes": 120,
	})
	p.takeCues(turnCueCtx("outer", []string{"outer"})) // 记住上一次心跳的可读域
	p.mu.Lock()
	p.lastBeat = time.Now()
	base := p.nextBeatLocked()
	p.mu.Unlock()

	cue.Post(cue.Cue{Source: "agenda", Key: "inner", Domain: "inner", Text: "里", Expire: time.Now().Add(time.Hour)})
	p.mu.Lock()
	notPulled := p.nextBeatLocked()
	p.mu.Unlock()
	if notPulled.Before(base) {
		t.Errorf("别的域的理由不该把这一轮提前: base=%v got=%v", base, notPulled)
	}

	cue.Post(cue.Cue{Source: "agenda", Key: "outer", Domain: "outer", Text: "表", Expire: time.Now().Add(time.Hour)})
	p.mu.Lock()
	pulled := p.nextBeatLocked()
	p.mu.Unlock()
	if !pulled.Before(base) {
		t.Errorf("本域的理由应提前下一拍: base=%v got=%v", base, pulled)
	}
}

// 心跳在可见域裁决之后领取理由，取走即清；下一次没有理由时不再出现该段。
func TestTurnPromptCarriesCues(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, nil)

	postCue("你所在的杭州刚下起了小雨。")
	ctx := turnCueCtx("outer", []string{"outer"})
	text, err := p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "【值得开口的事】") || !strings.Contains(text, "刚下起了小雨") {
		t.Errorf("心跳提示应带上开口理由:\n%s", text)
	}
	p.settleCues(true)
	if cue.Pending(time.Now()) {
		t.Error("送达后公告板应已清空")
	}

	text, err = p.TurnPrompt(ctx, plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "值得开口的事") {
		t.Errorf("无理由时不该出现该段:\n%s", text)
	}
}

// 只领取本轮可读域的理由，其他域的留在公告板上等它自己那一轮。
func TestTurnPromptFiltersCuesByDomain(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, nil)
	cue.Post(cue.Cue{Source: "agenda", Key: "outer", Domain: "outer", Text: "表人格的约定", Expire: time.Now().Add(time.Hour)})
	cue.Post(cue.Cue{Source: "agenda", Key: "inner", Domain: "inner", Text: "里人格的约定", Expire: time.Now().Add(time.Hour)})

	outerText, err := p.TurnPrompt(turnCueCtx("outer", []string{"outer"}), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outerText, "表人格的约定") || strings.Contains(outerText, "里人格的约定") {
		t.Fatalf("表人格的提示混入了里人格的理由:\n%s", outerText)
	}
	p.settleCues(true)

	innerText, err := p.TurnPrompt(turnCueCtx("inner", []string{"outer", "inner"}), plugin.TurnEvent{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(innerText, "里人格的约定") {
		t.Fatalf("里人格那一轮没拿到自己的理由:\n%s", innerText)
	}
	p.settleCues(true)
}

// 轮次失败时把已领取的理由放回公告板，等下一拍再说。
func TestSettleCuesRestoresOnFailure(t *testing.T) {
	drainCues()
	defer drainCues()
	p, _ := newInited(t, noTurn, nil)
	postCue("下雨了")
	if _, err := p.TurnPrompt(turnCueCtx("outer", []string{"outer"}), plugin.TurnEvent{}); err != nil {
		t.Fatal(err)
	}
	if cue.Pending(time.Now()) {
		t.Fatal("TurnPrompt 之后理由应已被领取")
	}
	p.settleCues(false)
	if !cue.Pending(time.Now()) {
		t.Error("轮次失败时理由应被放回公告板")
	}
}
