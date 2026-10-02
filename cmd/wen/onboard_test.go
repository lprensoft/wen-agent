package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/modelcfg"
)

// onboardFixture 搭一个全新安装的配置目录：没有 config.yaml，也没有 models.json。
func onboardFixture(t *testing.T) (*config.Config, string, *offlineBackend) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".wen") // 与 ~/.wen 一样，起初并不存在
	path := filepath.Join(dir, "config.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("没有配置文件时 Load 不该失败: %v", err)
	}
	store, err := modelcfg.NewStore(i18n.LangZH, filepath.Join(dir, "models.json"), cfg)
	if err != nil {
		t.Fatalf("没有任何提供商时 NewStore 不该失败: %v", err)
	}
	return cfg, path, &offlineBackend{cfg: cfg, models: store}
}

var deepseekProvider = modelcfg.Provider{
	Name: "DeepSeek", Type: "openai_compat", BaseURL: "https://api.deepseek.com",
	APIKey: "sk-test", Models: []modelcfg.Model{{ID: "deepseek-v4-flash"}},
}

// commitOnboard 按向导的顺序落下全部答案：先存模型，再落端口与语言。
func commitOnboard(t *testing.T, b modelsBackend, cfg *config.Config, path string, port int, lang string, hasConfigFile bool) {
	t.Helper()
	doc, err := b.loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveOnboardModel(b, doc, deepseekProvider, "deepseek-v4-flash"); err != nil {
		t.Fatal(err)
	}
	applyStartupAnswers(cfg, path, port, lang, i18n.LangZH, hasConfigFile, false)
}

// TestOnboardDefaultsLeaveNoConfigFile 盯住这次改动的目标：新用户回答完向导、
// 端口与语言都取默认值时，一个 config.yaml 都不该有，模型只进 models.json。
func TestOnboardDefaultsLeaveNoConfigFile(t *testing.T) {
	cfg, path, b := onboardFixture(t)
	commitOnboard(t, b, cfg, path, config.DefaultPort, i18n.LangZH, false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("端口与语言都是默认值时不该生成 config.yaml")
	}
	if _, err := os.Stat(filepath.Join(cfg.BaseDir, "models.json")); err != nil {
		t.Errorf("models.json 应当写好: %v", err)
	}

	// 下一次启动：从同一个目录重新读，应当直接可用、不再进向导
	cfg2, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureModel(cfg2, path, false); err != nil {
		t.Errorf("向导之后再启动不该再要求配置: %v", err)
	}
}

// TestOnboardWritesOnlyWhatDiffers：端口被占、换了一个，或者选了和系统区域不同的
// 语言时，才写一份只含这几项的 config.yaml，并且当场改进 cfg 让服务接着用。
func TestOnboardWritesOnlyWhatDiffers(t *testing.T) {
	cfg, path, b := onboardFixture(t)
	commitOnboard(t, b, cfg, path, 8091, i18n.LangEN, false)
	if cfg.Server.Port != 8091 || cfg.Lang != i18n.LangEN {
		t.Errorf("cfg 应当带上向导定下的值: port=%d lang=%q", cfg.Server.Port, cfg.Lang)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Server.Port != 8091 || reloaded.Lang != i18n.LangEN {
		t.Errorf("config.yaml 没记下来: port=%d lang=%q", reloaded.Server.Port, reloaded.Lang)
	}
}

// TestOnboardNeverRewritesConfigFile：老用户已有 config.yaml 时，向导只动 models.json。
func TestOnboardNeverRewritesConfigFile(t *testing.T) {
	cfg, path, b := onboardFixture(t)
	original := "# 我自己的注释\nserver:\n  port: 9000\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	commitOnboard(t, b, cfg, path, 0, i18n.LangEN, true)
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("已有的 config.yaml 被改写了:\n%s", got)
	}
}

func TestNextFreePortSkipsTakenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port

	if portFree("127.0.0.1", taken) {
		t.Fatalf("端口 %d 正被占用，portFree 却说空着", taken)
	}
	alt := nextFreePort("127.0.0.1", taken)
	if alt == 0 || alt == taken {
		t.Errorf("nextFreePort(%d) = %d，应当给出另一个空着的端口", taken, alt)
	}
	if !portFree("127.0.0.1", alt) {
		t.Errorf("给出的端口 %s 并不空着", strconv.Itoa(alt))
	}
}

// TestEnsureModelHeadlessExplains：不在终端里（测试本身就是）且没配模型时，不能卡住等输入，
// 要报一句该怎么办。
func TestEnsureModelHeadlessExplains(t *testing.T) {
	cfg, path, _ := onboardFixture(t)
	err := ensureModel(cfg, path, false)
	if err == nil || !strings.Contains(err.Error(), "wen onboard") {
		t.Errorf("应当提示去跑 wen onboard，得到 %v", err)
	}
}

// TestEnsureModelBlamesUnsetEnvVar：config.yaml 里的提供商只是密钥为空（${VAR} 没设）时，
// 不进向导——向导只会再加一家提供商、把明文密钥存进 models.json，此后永久压过 config.yaml。
// 要直接说出是哪个提供商。
func TestEnsureModelBlamesUnsetEnvVar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`
model:
  provider: mine
  name: some-model
providers:
  mine:
    type: openai_compat
    base_url: https://api.example.com/v1
    api_key: ${WEN_TEST_SURELY_UNSET}
`), 0o644)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	err = ensureModel(cfg, path, false)
	if err == nil || !strings.Contains(err.Error(), `"mine"`) || strings.Contains(err.Error(), "wen onboard") {
		t.Errorf("应当点名缺密钥的提供商而不是引向向导，得到 %v", err)
	}
}

// TestLegacyConfigWithoutModelSection：旧版 Default() 带着 model.provider: deepseek，
// 只写了 providers.deepseek 的 config.yaml 因此能用。升级之后它必须照样能用。
func TestLegacyConfigWithoutModelSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`
providers:
  deepseek:
    type: openai_compat
    base_url: https://api.deepseek.com
    api_key: sk-test
`), 0o644)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureModel(cfg, path, false); err != nil {
		t.Errorf("旧配置升级后应当照常可用: %v", err)
	}
}
