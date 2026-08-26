package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"wen/internal/i18n"
	"wen/internal/oslocale"

	"gopkg.in/yaml.v3"

	"wen/internal/llm"
)

type Config struct {
	// Lang 是进程级的兜底语言（zh / en / …），决定没有 HTTP 请求可依附的那些路径
	// 用哪门语言：模型收到的提示词、插件的状态行、会话注记、运行日志，以及
	// wen status 这类一次性命令。留空表示跟随操作系统的区域设置。
	//
	// Web UI 与 wen config 仍按请求判定（?lang= 优先于 Accept-Language），
	// 这里的值只是它们判不出来时的最后一档——同一个服务可能一台电脑一台手机各用
	// 一种语言，服务端不该替谁记住界面语言。
	Lang      string                    `yaml:"lang"`
	Server    ServerConfig              `yaml:"server"`
	Model     ModelConfig               `yaml:"model"`
	Providers map[string]ProviderConfig `yaml:"providers"`
	Agent     AgentConfig               `yaml:"agent"`
	Session   SessionConfig             `yaml:"session"`

	// BaseDir 是配置文件所在目录，用于解析相对路径（不在 YAML 中）。
	BaseDir string `yaml:"-"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// TrustLoopback 决定来自回环地址的请求是否免认证（默认开启）。
	// 指针是为了与「未配置」区分。套反向代理时必须显式关掉：那种部署下所有请求
	// 都从回环地址进来，开着等于没有认证。
	TrustLoopback *bool `yaml:"trust_loopback"`
}

// TrustLoopbackOrDefault 返回回环免认证的实际取值（未配置按开启算）。
func (c ServerConfig) TrustLoopbackOrDefault() bool {
	return c.TrustLoopback == nil || *c.TrustLoopback
}

type ModelConfig struct {
	Provider    string  `yaml:"provider"`
	Name        string  `yaml:"name"`
	Temperature float64 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
	// Thinking 思考模式：off / low / medium / high / xhigh / max
	//（DeepSeek 服务端将 medium、xhigh 归并为 high）。思考开启时 temperature 不生效。
	Thinking string `yaml:"thinking"`
	// ContextLength 模型上下文窗口（token 数），超出预算时裁剪最旧的对话轮次
	ContextLength int `yaml:"context_length"`
}

type ProviderConfig struct {
	Type    string `yaml:"type"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	// ThinkingDialect 是思考参数方言（openai_compat 模式专用）：
	// deepseek（默认）/ minimax / qwen / effort / none。
	ThinkingDialect string `yaml:"thinking_dialect"`
	// PromptCache 是否使用提示词缓存（anthropic 模式专用，默认开启）。
	// 指针是为了与「未配置」区分：不写这一项按开启算。
	PromptCache *bool `yaml:"prompt_cache"`
}

type AgentConfig struct {
	SystemPrompt string `yaml:"system_prompt"`
	MaxTurns     int    `yaml:"max_turns"`
	Workdir      string `yaml:"workdir"` // 工具与环境块使用的工作目录，空 = 进程当前目录
}

type SessionConfig struct {
	Dir string `yaml:"dir"`
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 8080},
		Model: ModelConfig{
			Provider:      "deepseek",
			Name:          "deepseek-v4-flash",
			Temperature:   0.7,
			MaxTokens:     4096,
			Thinking:      "high",
			ContextLength: 1000000,
		},
		Providers: map[string]ProviderConfig{
			"deepseek": {
				Type:    "openai_compat",
				BaseURL: "https://api.deepseek.com",
				APIKey:  "${DEEPSEEK_API_KEY}",
			},
		},
		Agent: AgentConfig{
			SystemPrompt: "",
			MaxTurns:     20,
		},
	}
}

// warnLegacyPlugins 提醒用户配置文件里的 plugins 段已不再生效。
//
// 插件的开关与配置改为只由界面管理、只存 plugins.state.json：两处都能配的时候，
// 到底哪一份在生效需要记住一条优先级规则，而界面上改过的值又不回写配置文件，
// 于是配置文件里的内容会慢慢变成误导。这里只警告不报错——不该因为一段过时的配置
// 就让程序起不来。
func warnLegacyPlugins(path string, raw []byte) {
	// 语言只能自己再认一次：这句警告在 Load 里打出来，而 Load 正是那个还没算出
	// 语言的地方（配置刚读进来、外面的 adoptLang 还没跑）。PeekLang 只读 lang: 一个键。
	lang := i18n.Match(PeekLang(path), oslocale.Current())
	var probe struct {
		Plugins map[string]any `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil || len(probe.Plugins) == 0 {
		return
	}
	i18n.Logf(lang, "log.config.legacy_plugins", "提示: %s 中的 plugins 段已不再生效，可以删掉。"+
		"插件的开关与配置请在设置页调整（存于 <配置目录>/plugins.state.json）", path)
}

// ResolvePath 确定配置文件路径：显式 flag > ./config.yaml > ~/.wen/config.yaml。
// 返回路径可能不存在（此时 Load 使用默认配置）。
func ResolvePath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".wen", "config.yaml")
}

// Load 加载配置：读取 path 指向的 YAML（不存在则用默认值）。
// 值中的 ${VAR} 占位符会替换为进程环境变量（api_key 推荐直接写在配置里）。
// PeekLang 只把配置文件里的 lang: 读出来，不做任何校验，出任何差错都答空串。
//
// 给参数分发之前的那一步用：帮助文本与未知命令的提示要在 Load 之前打出来，而 Load
// 会校验模型配置（缺 api_key 直接报错）、还会为遗留的 plugins: 段打一行警告——为了
// 认一个语言名把这些副作用提前触发一遍是不划算的，况且 wen help 本来就该在配置还
// 不完整时也能用。
func PeekLang(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var probe struct {
		Lang string `yaml:"lang"`
	}
	if err := yaml.Unmarshal(expandEnv(raw), &probe); err != nil {
		return ""
	}
	return probe.Lang
}

func Load(path string) (*Config, error) {
	cfg := Default()

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfg.BaseDir = filepath.Dir(abs)

	raw, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		expandInPlace(cfg)
		return cfg, cfg.validate()
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := yaml.Unmarshal(expandEnv(raw), cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	warnLegacyPlugins(path, raw)
	expandInPlace(cfg)
	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	p, ok := c.Providers[c.Model.Provider]
	if !ok {
		return fmt.Errorf("model.provider %q not found in providers", c.Model.Provider)
	}
	if !llm.IsKnownType(p.Type) {
		return fmt.Errorf("provider %q: unsupported type %q (%s)",
			c.Model.Provider, p.Type, strings.Join(llm.KnownTypes, "/"))
	}
	if p.APIKey == "" {
		return fmt.Errorf("provider %q: api_key is empty (set it in config.yaml)", c.Model.Provider)
	}
	if c.Agent.MaxTurns <= 0 {
		c.Agent.MaxTurns = 20
	}
	switch c.Model.Thinking {
	case "":
		c.Model.Thinking = "high"
	case "off", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("model.thinking %q invalid (off/low/medium/high/xhigh/max)", c.Model.Thinking)
	}
	if c.Model.ContextLength <= 0 {
		c.Model.ContextLength = 1000000
	}
	return nil
}

// SessionDir 返回 session 存储目录，未配置时为 <BaseDir>/sessions。
func (c *Config) SessionDir() string {
	if c.Session.Dir != "" {
		if filepath.IsAbs(c.Session.Dir) {
			return c.Session.Dir
		}
		return filepath.Join(c.BaseDir, c.Session.Dir)
	}
	return filepath.Join(c.BaseDir, "sessions")
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv 将文本中的 ${VAR} 替换为环境变量值，未设置的变量替换为空串。
func expandEnv(b []byte) []byte {
	return envPattern.ReplaceAllFunc(b, func(m []byte) []byte {
		name := envPattern.FindSubmatch(m)[1]
		return []byte(os.Getenv(string(name)))
	})
}

// expandInPlace 对默认配置中残留的 ${VAR} 占位符做替换（配置文件缺失或未覆盖该字段时）。
func expandInPlace(c *Config) {
	for name, p := range c.Providers {
		p.APIKey = string(expandEnv([]byte(p.APIKey)))
		p.BaseURL = string(expandEnv([]byte(p.BaseURL)))
		c.Providers[name] = p
	}
}
