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

// 两家提供商可以合法地只差「ToLower 相同、EqualFold 不同」的大小写（İstanbul 与
// istanbul），各有一个同名模型时它们是两个不同的目标。备用链去重若再折一次小写，
// 会把备用项误判成主模型的重复项丢掉，主模型故障时因此不会切换。同一家换个大小写
// 写进备用项的仍应去重——resolveSelection 返回的是条目里的实际名字。
func TestResolveChainKeepsToLowerCollidingProviders(t *testing.T) {
	s, path := newStore(t, baseConfig())

	next := s.Snapshot()
	next.Providers = append(next.Providers,
		Provider{Name: "İstanbul", Type: "openai_compat", BaseURL: "https://a.example.com",
			APIKey: "k", Models: []Model{{ID: "m"}}},
		Provider{Name: "istanbul", Type: "openai_compat", BaseURL: "https://b.example.com",
			APIKey: "k", Models: []Model{{ID: "m"}}},
	)
	next.Current = Selection{Provider: "İstanbul", Model: "m"}
	next.Fallbacks = []Selection{
		{Provider: "istanbul", Model: "m"},
		{Provider: "Istanbul", Model: "m"}, // 解析后就是 istanbul，与上一条重复
	}
	if _, err := s.Save("", next); err != nil {
		t.Fatalf("Save: %v", err)
	}

	chain, err := s.ResolveChain("")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("两家实际不同的提供商都应保留（大小写写法的重复项仍去重）: %+v", chain)
	}
	if chain[0].ProviderName != "İstanbul" || chain[1].ProviderName != "istanbul" {
		t.Fatalf("chain = %+v", chain)
	}

	// 新版本保存允许这种组合，载入必须同样接受——否则一次成功保存会把下次启动挡住。
	reloaded, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatalf("重新载入: %v", err)
	}
	chain, err = reloaded.ResolveChain("")
	if err != nil || len(chain) != 2 {
		t.Fatalf("载入后链应仍是两家: chain=%+v err=%v", chain, err)
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
