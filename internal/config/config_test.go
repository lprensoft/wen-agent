package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsWhenFileMissing(t *testing.T) {
	// 没有配置文件不是错误：模型由首次运行向导配进 models.json，这里只给启动期默认值
	cfg, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 8080 || cfg.Model.Thinking != "high" || cfg.Model.MaxTokens != 4096 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	// 不再内置任何提供商：此前写死的 DeepSeek + ${DEEPSEEK_API_KEY} 是一条没写进
	// 文档的第二入口
	if len(cfg.Providers) != 0 || cfg.Model.Provider != "" {
		t.Errorf("默认配置不该带提供商: %+v / %+v", cfg.Providers, cfg.Model)
	}
}

func TestLoadAcceptsEmptyAPIKey(t *testing.T) {
	// 密钥为空不再在这一层报错：models.json 里可能已经配好了别的提供商，
	// 「有没有能用的模型」要等两层合并之后才判得了
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`
model:
  provider: deepseek
  name: deepseek-v4-flash
providers:
  deepseek:
    type: openai_compat
    base_url: https://api.deepseek.com
    api_key: ${WEN_TEST_UNSET_KEY}
`), 0o644)
	if _, err := Load(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatalf("空密钥不该让 Load 失败: %v", err)
	}
}

func TestCreateMinimal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if err := CreateMinimal(path, "第一行\n第二行", 8090, "en"); err != nil {
		t.Fatalf("CreateMinimal: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 8090 || cfg.Lang != "en" {
		t.Errorf("写进去的值没读回来: port=%d lang=%q", cfg.Server.Port, cfg.Lang)
	}
	// 只新建、不改写
	if err := CreateMinimal(path, "x", 9000, ""); err == nil {
		t.Error("文件已存在时应拒绝，不该覆盖用户的配置")
	}
}

func TestLegacyPluginsSectionIgnoredNotFatal(t *testing.T) {
	// 插件配置改为只由界面管理。老配置文件里残留的 plugins 段应被忽略，
	// 但不能因此让程序起不来——只在启动日志里提醒一句。
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`
model:
  provider: deepseek
providers:
  deepseek:
    type: openai_compat
    base_url: https://api.deepseek.com
    api_key: sk-x
plugins:
  exec_command:
    enabled: false
    config:
      guard: off
  no_such_plugin:
    enabled: true
`), 0o644)

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("残留的 plugins 段不该导致加载失败: %v", err)
	}
	if cfg.Model.Provider != "deepseek" {
		t.Errorf("其余配置应照常生效: %+v", cfg.Model)
	}
}

func TestLoadYAMLWithInlineKey(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`
server:
  port: 9999
model:
  provider: deepseek
  name: deepseek-reasoner
providers:
  deepseek:
    type: openai_compat
    base_url: https://api.deepseek.com
    api_key: sk-inline-key
`), 0o644)

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("port = %d, want 9999", cfg.Server.Port)
	}
	if cfg.Model.Name != "deepseek-reasoner" {
		t.Errorf("model = %q", cfg.Model.Name)
	}
	if got := cfg.Providers["deepseek"].APIKey; got != "sk-inline-key" {
		t.Errorf("api_key = %q, want inline value", got)
	}
	// 未覆盖的字段保留默认值
	if cfg.Model.Temperature != 0.7 || cfg.Agent.MaxTurns != 20 {
		t.Errorf("unset fields lost defaults: %+v", cfg)
	}
}

func TestLoadYAMLWithEnvVarPlaceholder(t *testing.T) {
	t.Setenv("WEN_TEST_KEY", "sk-from-env")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`
providers:
  deepseek:
    type: openai_compat
    base_url: https://api.deepseek.com
    api_key: ${WEN_TEST_KEY}
`), 0o644)

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["deepseek"].APIKey; got != "sk-from-env" {
		t.Errorf("api_key = %q, want value from env var", got)
	}
}

func TestValidateRejectsMissingProvider(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`
model:
  provider: nope
`), 0o644)
	if _, err := Load(filepath.Join(dir, "config.yaml")); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestValidateAcceptsAnthropicType(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`
model:
  provider: claude
providers:
  claude:
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: sk-ant-x
`), 0o644)
	if _, err := Load(filepath.Join(dir, "config.yaml")); err != nil {
		t.Fatalf("anthropic type should be accepted: %v", err)
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`
model:
  provider: local
providers:
  local:
    type: ollama
    base_url: http://127.0.0.1:11434
    api_key: x
`), 0o644)
	if _, err := Load(filepath.Join(dir, "config.yaml")); err == nil {
		t.Fatal("expected error for unsupported provider type")
	}
}

func TestSessionDirDefault(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(dir, "sessions")
	if got := cfg.SessionDir(); got != want {
		t.Errorf("SessionDir = %q, want %q", got, want)
	}
}
