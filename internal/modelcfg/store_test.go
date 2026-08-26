package modelcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wen/internal/config"
)

func testConfig(providers map[string]config.ProviderConfig, model config.ModelConfig) *config.Config {
	return &config.Config{Providers: providers, Model: model}
}

// 默认基线：config.yaml 里一个 deepseek 提供商，当前模型 deepseek-chat。
func baseConfig() *config.Config {
	return testConfig(
		map[string]config.ProviderConfig{
			"deepseek": {Type: "openai_compat", BaseURL: "https://api.deepseek.com", APIKey: "sk-cfg"},
		},
		config.ModelConfig{
			Provider: "deepseek", Name: "deepseek-chat",
			Temperature: 0.7, MaxTokens: 4096, Thinking: "high", ContextLength: 1000000,
		},
	)
}

func newStore(t *testing.T, cfg *config.Config) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	s, err := NewStore("", path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func intp(v int) *int           { return &v }
func strp(v string) *string     { return &v }
func floatp(v float64) *float64 { return &v }

func TestInitFromConfigWhenFileMissing(t *testing.T) {
	s, path := newStore(t, baseConfig())

	view := s.Snapshot()
	if len(view.Providers) != 1 || view.Providers[0].Name != "deepseek" {
		t.Fatalf("providers = %+v", view.Providers)
	}
	if view.Providers[0].Source != "config" {
		t.Errorf("source = %q, want config", view.Providers[0].Source)
	}
	if len(view.Providers[0].Models) != 1 || view.Providers[0].Models[0].ID != "deepseek-chat" {
		t.Errorf("seed model = %+v", view.Providers[0].Models)
	}
	if view.Current != (Selection{Provider: "deepseek", Model: "deepseek-chat"}) {
		t.Errorf("current = %+v", view.Current)
	}
	// 懒创建：没改过东西就不该落盘
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("models.json should not be created before the first save")
	}
}

func TestOverlayOverridesConfigProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	os.WriteFile(path, []byte(`{
      "version": 1,
      "providers": [{"name":"deepseek","type":"openai_compat","base_url":"https://proxy.local",
                     "api_key":"sk-overlay","models":[{"id":"deepseek-reasoner"}]}],
      "current": {"provider":"deepseek","model":"deepseek-reasoner"}
    }`), 0o600)

	s, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	p := s.Snapshot().Providers[0]
	if p.BaseURL != "https://proxy.local" || p.APIKey != "sk-overlay" || p.Source != "" {
		t.Errorf("overlay should fully override config entry: %+v", p)
	}
	if len(p.Models) != 1 || p.Models[0].ID != "deepseek-reasoner" {
		t.Errorf("models = %+v", p.Models)
	}
}

func TestConfigOnlyProviderIsVisible(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers["zhipu"] = config.ProviderConfig{Type: "openai_compat", BaseURL: "https://open.bigmodel.cn", APIKey: "sk-z"}

	s, _ := newStore(t, cfg)
	view := s.Snapshot()
	if len(view.Providers) != 2 {
		t.Fatalf("providers = %+v", view.Providers)
	}
	if _, ok := findProvider(view.Providers, "zhipu"); !ok {
		t.Error("config.yaml 新增的提供商应当出现在列表中")
	}
}

func TestRemovedTombstone(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers["zhipu"] = config.ProviderConfig{Type: "openai_compat", BaseURL: "https://open.bigmodel.cn", APIKey: "sk-z"}
	s, path := newStore(t, cfg)

	// 删除 config.yaml 来的 zhipu：整档提交里不带它
	next := s.Snapshot()
	next.Providers = next.Providers[:0]
	for _, p := range s.Snapshot().Providers {
		if p.Name != "zhipu" {
			next.Providers = append(next.Providers, p)
		}
	}
	if _, err := s.Save("", next); err != nil {
		t.Fatal(err)
	}
	if _, ok := findProvider(s.Snapshot().Providers, "zhipu"); ok {
		t.Fatal("deleted provider should disappear")
	}

	// 重启后不能复活
	s2, err := NewStore("", path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findProvider(s2.Snapshot().Providers, "zhipu"); ok {
		t.Error("tombstoned provider must not come back on restart")
	}
}

func TestFieldFallback(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers[0].Models = []Model{
		{ID: "deepseek-chat"},
		{ID: "custom", MaxTokens: intp(8192), Thinking: strp("low"), Temperature: floatp(0), ContextLength: intp(128000)},
	}
	if _, err := s.Save("", next); err != nil {
		t.Fatal(err)
	}

	// 留空 → 取 config.yaml 的全局值
	r, err := s.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxTokens != 4096 || r.Thinking != "high" || r.Temperature != 0.7 || r.ContextLength != 1000000 {
		t.Errorf("fallback resolved = %+v", r)
	}

	// 显式设置 → 覆盖；temperature 为 0 也必须生效（指针语义）
	if _, err := s.SetCurrent("", Selection{Provider: "deepseek", Model: "custom"}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Resolve("")
	if r.MaxTokens != 8192 || r.Thinking != "low" || r.ContextLength != 128000 {
		t.Errorf("override resolved = %+v", r)
	}
	if r.Temperature != 0 {
		t.Errorf("temperature = %v, want explicit 0", r.Temperature)
	}
}

func TestSaveIsAtomicAndReloads(t *testing.T) {
	s, path := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers = append(next.Providers, Provider{
		Name: "anthropic", Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant",
		Models: []Model{{ID: "claude-opus-4-5", Name: "Claude Opus 4.5"}},
	})
	if _, err := s.Save("", next); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should not remain after an atomic write")
	}
	s2, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := findProvider(s2.Snapshot().Providers, "anthropic")
	if !ok || p.APIKey != "sk-ant" || len(p.Models) != 1 {
		t.Errorf("reloaded provider = %+v (ok=%v)", p, ok)
	}
}

func TestSaveKeepsEmptyAPIKey(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers[0].APIKey = "" // 界面只拿得到掩码，留空表示不修改
	next.Providers[0].BaseURL = "https://proxy.local"
	if _, err := s.Save("", next); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().Providers[0].APIKey; got != "sk-cfg" {
		t.Errorf("api_key = %q, want carried over", got)
	}
}

func TestSaveDoesNotTakeOverUnchangedConfigProvider(t *testing.T) {
	cfg := baseConfig()
	s, path := newStore(t, cfg)

	next := s.Snapshot()
	next.Providers = append(next.Providers, Provider{
		Name: "anthropic", Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "sk-ant",
		Models: []Model{{ID: "claude-opus-4-5"}},
	})
	if _, err := s.Save("", next); err != nil {
		t.Fatal(err)
	}

	// 未改动的 deepseek 不应被写进 models.json，仍跟随 config.yaml
	raw, _ := os.ReadFile(path)
	var onDisk File
	json.Unmarshal(raw, &onDisk)
	if _, ok := findProvider(onDisk.Providers, "deepseek"); ok {
		t.Error("未改动的 config.yaml 提供商不应被固化进 models.json")
	}
	cfg.Providers["deepseek"] = config.ProviderConfig{Type: "openai_compat", BaseURL: "https://new.deepseek.com", APIKey: "sk-cfg2"}
	s2, err := NewStore("", path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := findProvider(s2.Snapshot().Providers, "deepseek")
	if p.BaseURL != "https://new.deepseek.com" {
		t.Errorf("config.yaml 的后续改动应当生效，got %q", p.BaseURL)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name  string
		apply func(f *File)
	}{
		{"重名（大小写不敏感）", func(f *File) {
			f.Providers = append(f.Providers, Provider{Name: "DeepSeek", Type: "openai_compat",
				BaseURL: "https://x.com", APIKey: "k", Models: []Model{{ID: "m"}}})
		}},
		{"非法 base_url", func(f *File) { f.Providers[0].BaseURL = "abc" }},
		{"未知类型", func(f *File) { f.Providers[0].Type = "ollama" }},
		{"空名称", func(f *File) { f.Providers[0].Name = "  " }},
		{"空模型 ID", func(f *File) { f.Providers[0].Models = []Model{{ID: " "}} }},
		{"非法思考档位", func(f *File) { f.Providers[0].Models[0].Thinking = strp("ultra") }},
		{"temperature 越界", func(f *File) { f.Providers[0].Models[0].Temperature = floatp(3) }},
		{"max_tokens 非正", func(f *File) { f.Providers[0].Models[0].MaxTokens = intp(0) }},
		{"提供商列表为空", func(f *File) { f.Providers = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newStore(t, baseConfig())
			next := s.Snapshot()
			tc.apply(&next)
			if _, err := s.Save("", next); err == nil {
				t.Fatalf("%s 应当被拒绝", tc.name)
			}
		})
	}
}

func TestCannotDeleteProviderInUse(t *testing.T) {
	s, path := newStore(t, baseConfig())
	next := s.Snapshot()
	next.Providers[0].Models = nil // 删掉正在使用的模型

	if _, err := s.Save("", next); err == nil {
		t.Fatal("删除正在使用的模型应当被拒绝")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("失败的保存不应写盘")
	}
	if len(s.Snapshot().Providers[0].Models) != 1 {
		t.Error("失败的保存不应改动内存状态")
	}
}

func TestSetCurrentRejectsEmptyKeyAndMissingTarget(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers["nokey"] = config.ProviderConfig{Type: "anthropic", BaseURL: "https://api.anthropic.com"}
	s, _ := newStore(t, cfg)

	next := s.Snapshot()
	for i := range next.Providers {
		if next.Providers[i].Name == "nokey" {
			next.Providers[i].Models = []Model{{ID: "claude-opus-4-5"}}
		}
	}
	if _, err := s.Save("", next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetCurrent("", Selection{Provider: "nokey", Model: "claude-opus-4-5"}); err == nil {
		t.Error("切到没有 API Key 的提供商应当被拒绝")
	}
	if _, err := s.SetCurrent("", Selection{Provider: "deepseek", Model: "不存在"}); err == nil {
		t.Error("切到不存在的模型应当被拒绝")
	}
}

func TestProviderOrderStable(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers["zhipu"] = config.ProviderConfig{Type: "openai_compat", BaseURL: "https://a.com", APIKey: "k"}
	cfg.Providers["moonshot"] = config.ProviderConfig{Type: "openai_compat", BaseURL: "https://b.com", APIKey: "k"}

	var first []string
	for i := 0; i < 5; i++ {
		s, _ := newStore(t, cfg)
		var names []string
		for _, p := range s.Snapshot().Providers {
			names = append(names, p.Name)
		}
		if i == 0 {
			first = names
			continue
		}
		if len(names) != len(first) {
			t.Fatalf("provider count changed: %v vs %v", names, first)
		}
		for j := range names {
			if names[j] != first[j] {
				t.Fatalf("order unstable: %v vs %v", names, first)
			}
		}
	}
}

func TestMaskKey(t *testing.T) {
	if got := MaskKey("sk-1234567890abcd"); got != "sk-1••••••••abcd" {
		t.Errorf("MaskKey = %q", got)
	}
	if got := MaskKey("short"); got != "••••••••" {
		t.Errorf("MaskKey(short) = %q", got)
	}
	if got := MaskKey(""); got != "" {
		t.Errorf("MaskKey(empty) = %q", got)
	}
}
