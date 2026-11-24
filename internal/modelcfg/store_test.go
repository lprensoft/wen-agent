package modelcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// intp / strp 在 presets.go 里（非测试代码同样要拼可选字段），这里只补 float 那个
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

// 旧版本用 ToLower 判重，曾把只差大小写的两个名字同时存进 models.json。升级后
// 加载不能因为新的 EqualFold 重名校验直接失败——那会把服务与配置界面一起挡在门外，
// 用户连重命名、把两家设置都保住的机会都没有。载入放行，保存仍严格。
func TestLoadAllowsLegacyCaseDuplicateProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	raw := `{
      "version": 1,
      "providers": [
        {"name":"Ζευς","type":"openai_compat","base_url":"https://a.example.com","api_key":"k",
         "models":[{"id":"m"}]},
        {"name":"ΖΕΥΣ","type":"openai_compat","base_url":"https://b.example.com","api_key":"k",
         "models":[{"id":"m"}]}
      ],
      "current": {"provider":"Ζευς","model":"m"}
    }`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatalf("旧文件应当能载入: %v", err)
	}
	view := s.Snapshot()
	var names []string
	for _, p := range view.Providers {
		names = append(names, p.Name)
	}
	if len(names) != 3 { // 两家旧提供商 + config.yaml 的 deepseek
		t.Fatalf("载入后应保留两家旧提供商，得到 %v", names)
	}
	// 不重命名直接保存：严格校验仍然拒绝，避免新的重名被写回磁盘。
	if _, err := s.Save("", view); err == nil {
		t.Fatal("未改名的重名配置不该保存成功")
	}
	// 改掉其中一家之后可以保存，两家的设置都保留。
	view.Providers[1].Name = "Ζευς 2"
	out, err := s.Save("", view)
	if err != nil {
		t.Fatalf("改名后应能保存: %v", err)
	}
	var got []string
	for _, p := range out.Providers {
		got = append(got, p.Name)
	}
	if !slices.Contains(got, "Ζευς") || !slices.Contains(got, "Ζευς 2") {
		t.Fatalf("改名保存后两家都应保留，得到 %v", got)
	}
}

// 改名保存时密钥必须跟着原条目走，而不是按名字（更不是 EqualFold）重新配对。
// 旧配置里 Ζευς 与 ΖΕΥΣ 可以共存；把第一家改名、界面回传的 api_key 全部留空，
// 保存后第一家的密钥仍是自己的，第二家也不会被写入第一家的密钥。
func TestSaveKeepsKeysWithRenamedDuplicateProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	raw := `{
      "version": 1,
      "providers": [
        {"name":"Ζευς","type":"openai_compat","base_url":"https://a.example.com","api_key":"sk-a",
         "models":[{"id":"m"}]},
        {"name":"ΖΕΥΣ","type":"openai_compat","base_url":"https://b.example.com","api_key":"sk-b",
         "models":[{"id":"m"}]}
      ],
      "current": {"provider":"Ζευς","model":"m"}
    }`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatalf("旧文件应当能载入: %v", err)
	}

	next := s.Snapshot() // 带 Ref；密钥按界面规则全部留空
	for i := range next.Providers {
		next.Providers[i].APIKey = ""
	}
	next.Providers[0].Name = "Zeus"
	next.Current.Provider = "Zeus"
	if _, err := s.Save("", next); err != nil {
		t.Fatalf("改名保存: %v", err)
	}

	reloaded, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatalf("重新载入: %v", err)
	}
	keys := map[string]string{}
	for _, p := range reloaded.Snapshot().Providers {
		keys[p.Name] = p.APIKey
	}
	if keys["Zeus"] != "sk-a" {
		t.Fatalf("改名后的第一家应保留自己的密钥，得到 %q", keys["Zeus"])
	}
	if keys["ΖΕΥΣ"] != "sk-b" {
		t.Fatalf("第二家不该被写入第一家的密钥，得到 %q", keys["ΖΕΥΣ"])
	}
	// Ref 只是响应里的往返标识，不能落盘。
	if body, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(body), `"ref"`) {
		t.Fatalf("models.json 里不该出现 ref: %s", body)
	}
}

// 旧版本按 ToLower 判重时留下的文件：两家只差大小写、模型 ID 相同、密钥不同。
func legacyGreekStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	raw := `{
      "version": 1,
      "providers": [
        {"name":"Ζευς","type":"openai_compat","base_url":"https://a.example.com","api_key":"sk-a",
         "models":[{"id":"m"}]},
        {"name":"ΖΕΥΣ","type":"openai_compat","base_url":"https://b.example.com","api_key":"sk-b",
         "models":[{"id":"m"}]}
      ],
      "current": {"provider":"Ζευς","model":"m"}
    }`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatalf("旧文件应当能载入: %v", err)
	}
	return s, path
}

// 同一份改名请求提交两次——第一次成功但响应丢了，用户点了重试。第二次带回的 ref
// 已经对不上（那一家的名字改了）：必须拒绝，而不是回填不上就把空密钥写下去。
func TestSaveRejectsStaleRefInsteadOfClearingKey(t *testing.T) {
	s, path := legacyGreekStore(t)

	next := s.Snapshot()
	for i := range next.Providers {
		next.Providers[i].APIKey = ""
	}
	next.Providers[0].Name = "Zeus"
	next.Current.Provider = "Zeus"
	if _, err := s.Save("", next); err != nil {
		t.Fatalf("首次保存: %v", err)
	}
	if _, err := s.Save("", next); err == nil {
		t.Fatal("重试同一份请求时旧 ref 已失效，应被拒绝")
	}

	reloaded, err := NewStore("", path, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range reloaded.Snapshot().Providers {
		if p.APIKey == "" {
			t.Fatalf("被拒绝的重试不该把 %q 的密钥清掉", p.Name)
		}
	}
}

// 测试连接沿用哪一家的密钥：名字按 EqualFold 对上两家时，不带 ref 分不清就报错，
// 带了 ref 只认 ref；名字唯一时不带 ref 也照常取到。
func TestLookupPicksSavedProviderByRef(t *testing.T) {
	s, _ := legacyGreekStore(t)
	view := s.Snapshot()
	second := view.Providers[1].Ref

	if _, _, _, err := s.Lookup("", "", "ΖΕΥΣ", "m"); err == nil {
		t.Fatal("两家只差大小写又不带 ref，不该替用户挑一家的密钥")
	}
	if _, _, _, err := s.Lookup("", "gone", "ΖΕΥΣ", "m"); err == nil {
		t.Fatal("ref 对不上任何条目时应报错")
	}
	p, m, ok, err := s.Lookup("", second, "ΖΕΥΣ", "m")
	if err != nil || !ok {
		t.Fatalf("带 ref 取第二家: ok=%v err=%v", ok, err)
	}
	if p.APIKey != "sk-b" || p.BaseURL != "https://b.example.com" || m.ID != "m" {
		t.Fatalf("带 ref 应取到第二家自己保存的值: %+v", p)
	}

	single, _ := newStore(t, baseConfig())
	if p, _, ok, err := single.Lookup("", "", "DeepSeek", "deepseek-chat"); err != nil || !ok || p.APIKey != "sk-cfg" {
		t.Fatalf("名字唯一时不带 ref 也应取到: ok=%v err=%v %+v", ok, err, p)
	}
	if _, _, ok, err := single.Lookup("", "", "nobody", "m"); ok || err != nil {
		t.Fatalf("没有这个名字应是「没找到」而不是报错: ok=%v err=%v", ok, err)
	}
}

// 载入的宽松只对旧版曾接受的名称组合生效：两个都叫 Alpha（或普通 ASCII 大小写
// 变体）旧版就会拒绝，只可能来自手改；按名字查找只会静默命中第一家，载入即报错。
func TestLoadRejectsExactDuplicateProviders(t *testing.T) {
	for _, pair := range [][2]string{{"Alpha", "Alpha"}, {"Alpha", "alpha"}} {
		path := filepath.Join(t.TempDir(), "models.json")
		raw := fmt.Sprintf(`{
      "version": 1,
      "providers": [
        {"name":%q,"type":"openai_compat","base_url":"https://a.example.com","api_key":"k","models":[{"id":"m"}]},
        {"name":%q,"type":"openai_compat","base_url":"https://b.example.com","api_key":"k","models":[{"id":"m"}]}
      ],
      "current": {"provider":%q,"model":"m"}
    }`, pair[0], pair[1], pair[0])
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStore("", path, baseConfig()); err == nil {
			t.Fatalf("%q 与 %q 重名，应在载入时被拒绝", pair[0], pair[1])
		}
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
		{"重名（Unicode 大小写折叠）", func(f *File) {
			// Ζευς 与 ΖΕΥΣ 只差大小写，但 ToLower 折不出同一个键（词尾 ς 与 σ 不同），
			// 而 EqualFold 与查价口径都认它们是同一个名字。
			f.Providers = append(f.Providers,
				Provider{Name: "Ζευς", Type: "openai_compat",
					BaseURL: "https://a.example.com", APIKey: "k", Models: []Model{{ID: "m"}}},
				Provider{Name: "ΖΕΥΣ", Type: "openai_compat",
					BaseURL: "https://b.example.com", APIKey: "k", Models: []Model{{ID: "m"}}})
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
