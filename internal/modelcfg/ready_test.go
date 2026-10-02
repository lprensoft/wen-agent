package modelcfg

import (
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/config"
)

// TestEmptyStoreLoadsButIsNotReady 盯住全新安装的形态：一个提供商都没有时，
// 载入不能失败（否则 wen config、wen status 都进不去），但 Resolve 必须报「不能用」，
// 服务启动前正是据此决定进首次运行向导。
func TestEmptyStoreLoadsButIsNotReady(t *testing.T) {
	s, _ := newStore(t, config.Default())
	if _, err := s.Resolve(""); err == nil {
		t.Fatal("没有任何提供商时 Resolve 应当报错")
	}
}

// TestFirstSaveCreatesConfigDir 盯住向导的落点：没有 config.yaml 时配置目录是 ~/.wen，
// 它可能还不存在，第一次保存要自己把它建出来。
func TestFirstSaveCreatesConfigDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not", "yet", "models.json")
	s, err := NewStore("", path, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Save("", File{
		Providers: []Provider{{Name: "DeepSeek", Type: "openai_compat", BaseURL: "https://api.deepseek.com",
			APIKey: "sk-test", Models: []Model{{ID: "deepseek-v4-flash"}}}},
		Current: Selection{Provider: "DeepSeek", Model: "deepseek-v4-flash"},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	r, err := s.Resolve("")
	if err != nil {
		t.Fatalf("保存之后应当可用: %v", err)
	}
	if r.ModelID != "deepseek-v4-flash" || r.MaxTokens != 4096 {
		t.Errorf("resolved = %+v", r)
	}
}

// TestMissingKeyIsNotReady：缺密钥从前由 config.yaml 那一层查，现在归 Resolve——
// 查在合并之前就只看得见一半配置。本机地址不要密钥。
func TestMissingKeyIsNotReady(t *testing.T) {
	cfg := baseConfig()
	p := cfg.Providers["deepseek"]
	p.APIKey = ""
	cfg.Providers["deepseek"] = p
	s, _ := newStore(t, cfg)
	if _, err := s.Resolve(""); err == nil {
		t.Error("缺密钥时 Resolve 应当报错")
	}

	local := testConfig(
		map[string]config.ProviderConfig{"ollama": {Type: "openai_compat", BaseURL: "http://localhost:11434/v1"}},
		config.ModelConfig{Provider: "ollama", Name: "qwen3"},
	)
	s, _ = newStore(t, local)
	if _, err := s.Resolve(""); err != nil {
		t.Errorf("本机推理服务不需要密钥: %v", err)
	}
}

// TestNoSelectionReportsNotConfigured：有提供商但一个模型都选不上时，报「没配置」，
// 而不是让人摸不着头脑的「未找到提供商 ""」。
func TestNoSelectionReportsNotConfigured(t *testing.T) {
	cfg := testConfig(
		map[string]config.ProviderConfig{"foo": {Type: "openai_compat", BaseURL: "https://api.example.com/v1", APIKey: "sk-x"}},
		config.ModelConfig{},
	)
	s, _ := newStore(t, cfg)
	_, err := s.Resolve("")
	if err == nil || strings.Contains(err.Error(), `""`) {
		t.Errorf("应当报「没配置」，得到 %v", err)
	}
}
