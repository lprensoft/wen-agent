package agent

import (
	"context"
	"encoding/json"
	"testing"

	"wen/internal/llm"
	"wen/internal/plugin"
)

func TestResponsesItemsPersistWithAssistantMessage(t *testing.T) {
	raw := json.RawMessage(`[{"type":"reasoning","id":"rs_1","encrypted_content":"cipher"},{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"你好"}]}]`)
	primary := &chainProvider{events: []llm.StreamEvent{
		{Type: llm.EventContentDelta, Content: "你好"},
		{Type: llm.EventDone, RawItems: raw, RawOrigin: "test|gpt"},
	}}
	backup := &chainProvider{}
	ag, id := fallbackAgent(t, primary, backup, &translatorPlugin{})

	ag.Run(plugin.WithInteractive(context.Background()), id, "hi", func(Event) {})

	_, history, err := ag.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range history {
		if m.Role != llm.RoleAssistant {
			continue
		}
		found = true
		if string(m.ResponsesItems) != string(raw) {
			t.Fatalf("ResponsesItems = %s, want %s", m.ResponsesItems, raw)
		}
		if m.ResponsesOrigin != "test|gpt" {
			t.Fatalf("ResponsesOrigin = %q", m.ResponsesOrigin)
		}
	}
	if !found {
		t.Fatal("no assistant message persisted")
	}
}

func TestEstimateTokensIncludesResponsesItems(t *testing.T) {
	plain := llm.Message{Role: llm.RoleAssistant, Content: "hi"}
	withItems := llm.Message{Role: llm.RoleAssistant, Content: "hi", ResponsesItems: json.RawMessage(`{"a":"0123456789"}`)}
	if estimateTokens(withItems) <= estimateTokens(plain) {
		t.Fatalf("estimateTokens should account for ResponsesItems: plain=%d items=%d",
			estimateTokens(plain), estimateTokens(withItems))
	}
}
