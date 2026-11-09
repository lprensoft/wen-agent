package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// chainProvider 是 fallback 测试用的模型：可以同步报错，也可以先流事件再报错。
type chainProvider struct {
	syncErr error
	events  []llm.StreamEvent
	calls   int
	reqs    []llm.ChatRequest
}

func (p *chainProvider) ChatStream(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls++
	p.reqs = append(p.reqs, req)
	if p.syncErr != nil {
		return nil, p.syncErr
	}
	ch := make(chan llm.StreamEvent)
	go func() {
		defer close(ch)
		for _, ev := range p.events {
			ch <- ev
		}
	}()
	return ch, nil
}

func contentEvents(text string) []llm.StreamEvent {
	var out []llm.StreamEvent
	for _, r := range text {
		out = append(out, llm.StreamEvent{Type: llm.EventContentDelta, Content: string(r)})
	}
	return out
}

func fallbackAgent(t *testing.T, primary, backup llm.Provider, tr *translatorPlugin) (*Agent, string) {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	ag := New(primary, newTestManager(t, tr), store, Options{Model: "primary-model", MaxTurns: 3})
	ag.SetModelTargets([]ModelTarget{
		{Provider: primary, Options: ModelOptions{Model: "primary-model", MaxTokens: 4096}, Label: "primary"},
		{Provider: backup, Options: ModelOptions{Model: "backup-model", MaxTokens: 4096}, Label: "backup"},
	})
	return ag, meta.ID
}

func TestFallbackOnTransientBeforeOutput(t *testing.T) {
	primary := &chainProvider{syncErr: &llm.APIError{Status: 500, Body: "down"}}
	backup := &chainProvider{events: contentEvents("备用回复")}
	tr := &translatorPlugin{text: "不该出现", ok: true}
	ag, id := fallbackAgent(t, primary, backup, tr)

	var deltas string
	var sawError bool
	ag.Run(plugin.WithInteractive(context.Background()), id, "hi", func(ev Event) {
		switch ev.Type {
		case EventDelta:
			deltas += ev.Content
		case EventError:
			sawError = true
		}
	})
	if sawError || deltas != "备用回复" {
		t.Fatalf("deltas=%q sawError=%v", deltas, sawError)
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Fatalf("calls primary=%d backup=%d", primary.calls, backup.calls)
	}
	if len(backup.reqs) != 1 || backup.reqs[0].Model != "backup-model" {
		t.Fatalf("backup request = %+v", backup.reqs)
	}
	if tr.gotErr != nil {
		t.Fatalf("translator should not run after a successful fallback, got %v", tr.gotErr)
	}
}

func TestFallbackOnStreamErrorBeforeContent(t *testing.T) {
	primary := &chainProvider{events: []llm.StreamEvent{
		{Type: llm.EventError, Err: &llm.APIError{Status: 429, Body: "rate limited"}},
	}}
	backup := &chainProvider{events: contentEvents("备用")}
	tr := &translatorPlugin{text: "不该出现", ok: true}
	ag, id := fallbackAgent(t, primary, backup, tr)

	var deltas string
	ag.Run(plugin.WithInteractive(context.Background()), id, "hi", func(ev Event) {
		if ev.Type == EventDelta {
			deltas += ev.Content
		}
	})
	if deltas != "备用" || primary.calls != 1 || backup.calls != 1 {
		t.Fatalf("deltas=%q primary=%d backup=%d", deltas, primary.calls, backup.calls)
	}
}

func TestNoFallbackAfterPartialOutput(t *testing.T) {
	primary := &chainProvider{events: append(contentEvents("半截"),
		llm.StreamEvent{Type: llm.EventError, Err: &llm.APIError{Status: 500, Body: "down"}})}
	backup := &chainProvider{events: contentEvents("备用")}
	tr := &translatorPlugin{text: "（走神）", ok: true}
	ag, id := fallbackAgent(t, primary, backup, tr)

	var deltas string
	ag.Run(plugin.WithInteractive(context.Background()), id, "hi", func(ev Event) {
		if ev.Type == EventDelta {
			deltas += ev.Content
		}
	})
	if backup.calls != 0 {
		t.Fatal("partial output must disable fallback")
	}
	if !strings.HasSuffix(deltas, "\n\n（走神）") {
		t.Fatalf("deltas = %q", deltas)
	}
	if tr.gotErr == nil || !strings.Contains(tr.gotErr.Error(), "status 500") {
		t.Fatalf("translator err = %v", tr.gotErr)
	}
}

func TestAllTransientFailuresUseTranslator(t *testing.T) {
	primary := &chainProvider{syncErr: &llm.APIError{Status: 500, Body: "down"}}
	backup := &chainProvider{syncErr: &llm.APIError{Status: 503, Body: "still down"}}
	tr := &translatorPlugin{text: "（走神）……刚才说到哪了？", ok: true}
	ag, id := fallbackAgent(t, primary, backup, tr)

	var deltas string
	var sawError bool
	ag.Run(plugin.WithInteractive(context.Background()), id, "hi", func(ev Event) {
		switch ev.Type {
		case EventDelta:
			deltas += ev.Content
		case EventError:
			sawError = true
		}
	})
	if sawError || deltas != "（走神）……刚才说到哪了？" {
		t.Fatalf("deltas=%q sawError=%v", deltas, sawError)
	}
	if tr.gotErr == nil || !strings.Contains(tr.gotErr.Error(), "status 503") {
		t.Fatalf("translator should receive the last error, got %v", tr.gotErr)
	}
}

func TestConfigErrorDoesNotFallback(t *testing.T) {
	primary := &chainProvider{syncErr: &llm.APIError{Status: 401, Body: "bad key"}}
	backup := &chainProvider{events: contentEvents("备用")}
	tr := &translatorPlugin{ok: false}
	ag, id := fallbackAgent(t, primary, backup, tr)

	var errText string
	ag.Run(plugin.WithInteractive(context.Background()), id, "hi", func(ev Event) {
		if ev.Type == EventError {
			errText = ev.Error
		}
	})
	if backup.calls != 0 {
		t.Fatal("config errors must not switch providers")
	}
	if !strings.Contains(errText, "status 401") {
		t.Fatalf("error = %q", errText)
	}
	if tr.gotErr == nil {
		t.Fatal("translator should have been consulted")
	}
}

func TestCompleteFallsBack(t *testing.T) {
	primary := &chainProvider{syncErr: &llm.APIError{Status: 500, Body: "down"}}
	backup := &chainProvider{events: contentEvents("辅助结果")}
	ag, _ := fallbackAgent(t, primary, backup, &translatorPlugin{})

	got, err := ag.Complete(context.Background(), "问题")
	if err != nil {
		t.Fatal(err)
	}
	if got != "辅助结果" || backup.calls != 1 || backup.reqs[0].Model != "backup-model" {
		t.Fatalf("got=%q backup calls=%d reqs=%+v", got, backup.calls, backup.reqs)
	}
}

func TestFallbackDisabledWhenNoBackup(t *testing.T) {
	primary := &chainProvider{syncErr: &llm.APIError{Status: 500, Body: "down"}}
	tr := &translatorPlugin{text: "（走神）", ok: true}
	store, _ := session.NewStore(t.TempDir())
	meta, _ := store.Create()
	ag := New(primary, newTestManager(t, tr), store, Options{Model: "only", MaxTurns: 3})

	if _, err := ag.Complete(context.Background(), "问题"); err == nil {
		t.Fatal("Complete should return the primary error")
	}
	_ = meta
}

func TestTransientErrorDetectionIsUsed(t *testing.T) {
	// 保护一下依赖关系：fallback 的判据不能退化成「所有错误都切」。
	if llm.IsTransientError(errors.New("boom")) {
		t.Fatal("plain errors must not be treated as transient")
	}
}
