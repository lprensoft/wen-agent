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
//
// 这里**不带任何提供商**：选哪家、填哪个密钥是首次运行向导（wen onboard）的事，
// 结果存 models.json。此前这里写死了一个 DeepSeek、密钥取 ${DEEPSEEK_API_KEY}，
// 于是「设了这个环境变量就能不写配置启动」成了一条没写进文档的第二入口，
// 而没设的人连 wen config 都进不去——校验在合并 models.json 之前就把进程拦下了。
// model 段只留全局默认参数，模型条目没有覆盖时回落到它们。
func Default() *Config {
	return &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: DefaultPort},
		Model: ModelConfig{
			Temperature:   0.7,
			MaxTokens:     4096,
			Thinking:      "high",
			ContextLength: 1000000,
		},
		Providers: map[string]ProviderConfig{},
		Agent: AgentConfig{
			SystemPrompt: "",
			MaxTurns:     20,
		},
	}
}

// DefaultPort 是不写配置文件时的监听端口。首次运行向导发现它被占用时会另挑一个，
// 并把选定的端口写进一份只有这一项的 config.yaml。
const DefaultPort = 8080

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
// 返回路径可能不存在（此时 Load 使用默认配置）——配置文件是可选的，它所在的目录
// 照样是配置目录：models.json、会话与插件状态都落在那里。
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
// 会校验配置、还会为遗留的 plugins: 段打一行警告——为了认一个语言名把这些副作用
// 提前触发一遍是不划算的。
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
		return cfg, cfg.validate()
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := yaml.Unmarshal(expandEnv(raw), cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	warnLegacyPlugins(path, raw)
	legacyModelDefaults(cfg)
	return cfg, cfg.validate()
}

// legacyModelDefaults 补回旧版 Default() 给的那两个模型默认值，只对写了 config.yaml 的人生效。
//
// 旧版的内置默认是 model.provider: deepseek、model.name: deepseek-v4-flash，于是一份
// 只写了 providers.deepseek、没写 model: 段（或只写了一半）的 config.yaml 也能用。
// 新版的 Default() 不再带它们（选哪家归首次运行向导），这里把它们补回来，否则那些
// 配置升级后会突然要求重新配置——systemd 下就是服务起不来。范围收得和旧行为一样窄：
// 只认名叫 deepseek 的提供商，别的名字旧版本来也不会自动选中。
//
// 模型名补的是 deepseek-flash 而不是旧版那个 deepseek-v4-flash：后者已经下线，
// DeepSeek 只是暂时把它转到 deepseek-flash（V4.1-Flash），补一个迟早失效的名字没有意义。
func legacyModelDefaults(c *Config) {
	if c.Model.Provider == "" {
		if _, ok := c.Providers["deepseek"]; ok {
			c.Model.Provider = "deepseek"
		}
	}
	if c.Model.Provider == "deepseek" && c.Model.Name == "" {
		c.Model.Name = "deepseek-flash"
	}
}

// validate 只校验 config.yaml 里**写了**的东西，并补齐缺省值。
//
// 「有没有一个能用的模型」不在这里判：模型配置是 config.yaml 与 models.json 两层
// 合并的结果，只看前一层就会把 models.json 里配好了的人拦在门外（此前正是如此）。
// 那件事归 modelcfg.Store.Resolve，判不过时由命令入口决定是进向导还是报错。
func (c *Config) validate() error {
	if c.Model.Provider != "" {
		p, ok := c.Providers[c.Model.Provider]
		if !ok {
			return fmt.Errorf("model.provider %q not found in providers", c.Model.Provider)
		}
		if !llm.IsKnownType(p.Type) {
			return fmt.Errorf("provider %q: unsupported type %q (%s)",
				c.Model.Provider, p.Type, strings.Join(llm.KnownTypes, "/"))
		}
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
	if c.Server.Port <= 0 {
		c.Server.Port = DefaultPort
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

// CreateMinimal 新建一份只含给定几项的 config.yaml，供首次运行向导记下它问到的
// 那几样启动期配置（端口、语言）。port 为 0、lang 为空的项不写。
//
// 只新建、不改写：文件已经存在就报错。config.yaml 是用户的文件，里面可能有注释与
// ${VAR}，程序从不回写它（模型配置因此另存 models.json）；新建一份则不存在这个问题。
// header 是写在最前面的注释，可以多行，由调用方按语言给出。
func CreateMinimal(path, header string, port int, lang string) error {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(header, "\n"), "\n") {
		b.WriteString("# " + line + "\n")
	}
	if lang != "" {
		fmt.Fprintf(&b, "\nlang: %s\n", lang)
	}
	if port != 0 {
		fmt.Fprintf(&b, "\nserver:\n  port: %d\n", port)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
