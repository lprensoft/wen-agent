package modelcfg

import (
	"strings"
	"testing"

	"wen/internal/config"
)

func TestFallbackChainPersistsAndResolves(t *testing.T) {
	cfg := baseConfig()
	s, path := newStore(t, cfg)

	next := s.Snapshot()
	next.Providers = append(next.Providers, Provider{
		Name: "backup", Type: "openai_compat", BaseURL: "https://backup.example.com", APIKey: "sk-backup",
		Models: []Model{{ID: "backup-large"}},
	})
	next.Fallbacks = []Selection{
		{Provider: "backup", Model: "backup-large"},
		{Provider: "deepseek", Model: "deepseek-chat"},
		{Provider: "backup", Model: "backup-large"},
	}
	if _, err := s.Save("", next); err != nil {
		t.Fatalf("Save: %v", err)
	}

	chain, err := s.ResolveChain("")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("chain length = %d, want 2 (primary + one unique fallback): %+v", len(chain), chain)
	}
	if chain[0].ProviderName != "deepseek" || chain[1].ProviderName != "backup" || chain[1].ModelID != "backup-large" {
		t.Fatalf("chain = %+v", chain)
	}

	reloaded, err := NewStore("", path, cfg)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	chain, err = reloaded.ResolveChain("")
	if err != nil {
		t.Fatalf("ResolveChain after reload: %v", err)
	}
	if len(chain) != 2 || chain[1].ProviderName != "backup" {
		t.Fatalf("fallbacks did not persist: %+v", chain)
	}
}

func TestFallbackRejectsUnknownSelection(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Fallbacks = []Selection{{Provider: "deepseek", Model: "missing-model"}}
	_, err := s.Save("", next)
	if err == nil || !strings.Contains(err.Error(), "missing-model") {
		t.Fatalf("Save should reject an unknown fallback model, got %v", err)
	}
}

func TestFallbackRejectsMissingKey(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers = append(next.Providers, Provider{
		Name: "nokey", Type: "openai_compat", BaseURL: "https://nokey.example.com",
		Models: []Model{{ID: "m"}},
	})
	next.Fallbacks = []Selection{{Provider: "nokey", Model: "m"}}
	if _, err := s.Save("", next); err == nil {
		t.Fatal("Save should reject a remote fallback provider without a key")
	}
}

func TestFallbackAcceptsConfigProvider(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers["zhipu"] = config.ProviderConfig{
		Type: "openai_compat", BaseURL: "https://open.bigmodel.cn", APIKey: "sk-z",
	}
	s, _ := newStore(t, cfg)

	next := s.Snapshot()
	// 给 config.yaml 来的提供商补一个模型，让它能当备用模型；保存后它仍可跟随配置文件。
	for i := range next.Providers {
		if next.Providers[i].Name == "zhipu" {
			next.Providers[i].Models = []Model{{ID: "glm-4"}}
		}
	}
	next.Fallbacks = []Selection{{Provider: "zhipu", Model: "glm-4"}}
	if _, err := s.Save("", next); err != nil {
		t.Fatalf("Save: %v", err)
	}
	chain, err := s.ResolveChain("")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || chain[1].ProviderName != "zhipu" {
		t.Fatalf("chain = %+v", chain)
	}
}

func TestSetCurrentRemovesDuplicateFallback(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers = append(next.Providers, Provider{
		Name: "backup", Type: "openai_compat", BaseURL: "https://backup.example.com", APIKey: "sk-backup",
		Models: []Model{{ID: "b"}},
	})
	next.Fallbacks = []Selection{{Provider: "backup", Model: "b"}}
	if _, err := s.Save("", next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCurrent("", Selection{Provider: "backup", Model: "b"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().Fallbacks; len(got) != 0 {
		t.Fatalf("fallback selected as primary should be removed: %+v", got)
	}
	chain, err := s.ResolveChain("")
	if err != nil || len(chain) != 1 || chain[0].ProviderName != "backup" {
		t.Fatalf("chain=%+v err=%v", chain, err)
	}
}
