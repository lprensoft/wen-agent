package modelcfg

import (
	"strings"
	"testing"

	"wen/internal/config"
	"wen/internal/llm"
)

func TestProviderProtocolPersistsAndResolves(t *testing.T) {
	s, path := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers[0].Protocol = llm.ProtocolResponses
	if _, err := s.Save("", next); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	view := reloaded.Snapshot()
	if view.Providers[0].Protocol != llm.ProtocolResponses {
		t.Fatalf("protocol = %q, want responses", view.Providers[0].Protocol)
	}
	r, err := reloaded.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if r.Protocol != llm.ProtocolResponses {
		t.Fatalf("resolved protocol = %q", r.Protocol)
	}
	if cfg := r.LLMConfig(""); cfg.Protocol != llm.ProtocolResponses {
		t.Fatalf("LLMConfig protocol = %q", cfg.Protocol)
	}
}

func TestProtocolFromConfigYAML(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers["custom"] = config.ProviderConfig{
		Type: "openai_compat", BaseURL: "https://custom.example.com/v1",
		APIKey: "sk-c", Protocol: llm.ProtocolResponses,
	}
	cfg.Model.Provider = "custom"
	cfg.Model.Name = "m"
	s, _ := newStore(t, cfg)
	r, err := s.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if r.Protocol != llm.ProtocolResponses {
		t.Fatalf("protocol from config.yaml = %q", r.Protocol)
	}
}

func TestProtocolResponsesRejectedOnAnthropic(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers = append(next.Providers, Provider{
		Name: "claude", Type: llm.TypeAnthropic, BaseURL: "https://api.anthropic.com",
		APIKey: "sk-ant", Protocol: llm.ProtocolResponses, Models: []Model{{ID: "claude"}},
	})
	_, err := s.Save("", next)
	if err == nil || !strings.Contains(err.Error(), "responses") {
		t.Fatalf("Save should reject responses on anthropic, got %v", err)
	}
}

func TestUnknownProtocolRejected(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers[0].Protocol = "grpc"
	if _, err := s.Save("", next); err == nil {
		t.Fatal("Save should reject an unknown protocol")
	}
}
