package modelcfg

import (
	"fmt"
	"strings"

	"wen/internal/i18n"
	"wen/internal/llm"
)

// 预置提供商：添加提供商时先从这张表里挑一家，挑不到才手工填。
//
// 它只是一份**填表单的初值**，不是第三种配置来源。用户选中一条之后保存下来的
// 就是一条普通的用户条目，与这张表再无关系——升级后表里多了个模型，他那边不会
// 自己长出来。这是刻意的：config.yaml 与 plugins.state.json 已经演示过「两处
// 都能配」会怎样，那条规则得被记住，而界面的改动又不回写，久了就成误导。
//
// 表里的每一项都是照官方文档核过的（2026-10）。模型 ID 是这份表里最短命的东西，
// 发版时随 CHANGELOG 一起复核一遍——新手拿到的「开箱即用」不该在某一版之后
// 变成「开箱报错」。
//
// 命名：有国内与国际两套账号的厂商各出一条，国际站占主名、国内那条缀 (CN)。
// 名字会存进 models.json 当唯一标识，所以它不进语言包——翻译了就对不上。
// 会翻译的只有 Desc 那一句。

// Preset 是一条预置提供商。
type Preset struct {
	// Key 是稳定标识，用来拼语言包的键与前端的 value，不展示。
	Key string
	// Name 是填进表单的提供商名，也是存盘后的唯一标识。
	Name string
	Type string
	// BaseURL 已按各家文档核过：openai_compat 那边我们直接拼 /chat/completions，
	// 所以该带 /v1 的必须带上（DeepSeek 与方舟是少数不带的）。
	BaseURL string
	Dialect string
	// Models 是开箱可用的模型清单，第一条即选中项。
	Models []Model
	// ConsoleURL 是取密钥的页面，界面上给一个链接，省得用户自己搜。
	ConsoleURL string
	// Desc 是一句话说明，中文原文，展示时经 i18n 按语言取。
	Desc string
}

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

// 提供商名里的 (CN) 后缀。抽成常量是因为它在下面出现五次，
// 而「国际站占主名」这条规矩改起来要一起改。
const cnSuffix = " (CN)"

// Presets 是预置表。顺序即界面上的顺序：同一厂商的两条挨着放（找起来按厂商找，
// 不按地区找），国内常用的排在前面，本地推理压末尾。
var presets = []Preset{
	{
		Key: "deepseek", Name: "DeepSeek", Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.deepseek.com", Dialect: llm.DialectDeepSeek,
		ConsoleURL: "https://platform.deepseek.com/api_keys",
		Desc:       "深度求索。注册即用，思考与非思考同一个模型。",
		Models: []Model{
			// deepseek-flash 是 V4.1-Flash，原生看图；旧的 deepseek-v4-flash 与
			// deepseek-v4-flash-vision-exp 已下线，官方只是暂时把它们转到这里
			{ID: "deepseek-flash", ContextLength: intp(1000000), MaxTokens: intp(65536)},
			{ID: "deepseek-v4-pro", ContextLength: intp(1000000), MaxTokens: intp(65536)},
		},
	},
	{
		Key: "zai_cn", Name: "Z.AI" + cnSuffix, Type: llm.TypeOpenAICompat,
		BaseURL: "https://open.bigmodel.cn/api/paas/v4", Dialect: llm.DialectGLM,
		ConsoleURL: "https://bigmodel.cn/usercenter/proj-mgmt/apikeys",
		Desc:       "智谱 GLM，国内站。glm-5.3 强制思考，关不掉。",
		Models:     glmModels(),
	},
	{
		Key: "zai", Name: "Z.AI", Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.z.ai/api/paas/v4", Dialect: llm.DialectGLM,
		ConsoleURL: "https://z.ai/manage-apikey/apikey-list",
		Desc:       "智谱 GLM，国际站。与国内站是两套账号，密钥不通用。",
		Models:     glmModels(),
	},
	{
		Key: "kimi_cn", Name: "Kimi" + cnSuffix, Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.moonshot.cn/v1", Dialect: llm.DialectKimi,
		ConsoleURL: "https://platform.kimi.com/console/api-keys",
		Desc:       "月之暗面 Kimi，国内站。K3 长上下文，原生视觉。",
		Models:     kimiModels(),
	},
	{
		Key: "kimi", Name: "Kimi", Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.moonshot.ai/v1", Dialect: llm.DialectKimi,
		ConsoleURL: "https://platform.kimi.ai/console/api-keys",
		Desc:       "月之暗面 Kimi，国际站。与国内站是两套账号，密钥不通用。",
		Models:     kimiModels(),
	},
	{
		Key: "qwen_cn", Name: "Qwen" + cnSuffix, Type: llm.TypeOpenAICompat,
		BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Dialect: llm.DialectQwen,
		ConsoleURL: "https://bailian.console.aliyun.com/cn-beijing/model/settings/api-key",
		Desc:       "通义千问，阿里云百炼。思考只有开关，没有档位。",
		Models:     qwenModels(),
	},
	{
		Key: "qwen", Name: "Qwen", Type: llm.TypeOpenAICompat,
		BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", Dialect: llm.DialectQwen,
		ConsoleURL: "https://modelstudio.console.alibabacloud.com/ap-southeast-1/model/settings/api-key",
		Desc:       "通义千问，新加坡站。与国内站是两套账号，密钥不通用。",
		Models:     qwenModels(),
	},
	{
		Key: "doubao", Name: "Doubao", Type: llm.TypeOpenAICompat,
		BaseURL: "https://ark.cn-beijing.volces.com/api/v3", Dialect: llm.DialectDeepSeek,
		ConsoleURL: "https://ark.volcengine.com/region:cn-beijing/apiKey",
		Desc:       "字节豆包，火山方舟。模型 ID 带版本日期，去掉就调不通。",
		Models: []Model{
			// 日期后缀是必须的，方舟不认不带日期的别名。这也意味着这两行
			// 会随版本过期，属于发版时该复核的部分。
			// pro 的上下文官方写作 1024k，精确的 token 数没有给出，取 1M 保守值
			{ID: "doubao-seed-2-1-pro-260915", ContextLength: intp(1000000), MaxTokens: intp(65536)},
			{ID: "doubao-seed-2-1-turbo-260628", ContextLength: intp(262144), MaxTokens: intp(65536)},
		},
	},
	{
		Key: "minimax_cn", Name: "MiniMax" + cnSuffix, Type: llm.TypeOpenAICompat,
		// 国内站的域名已换成 minimax.cn；旧的 api.minimaxi.com 仍能用，但文档里已不再出现
		BaseURL: "https://api.minimax.cn/v1", Dialect: llm.DialectMiniMax,
		ConsoleURL: "https://platform.minimax.cn/console/access?tab=api-keys",
		Desc:       "MiniMax，国内站。模型 ID 大小写敏感。",
		Models:     minimaxModels(),
	},
	{
		Key: "minimax", Name: "MiniMax", Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.minimax.io/v1", Dialect: llm.DialectMiniMax,
		ConsoleURL: "https://platform.minimax.io/console/access",
		Desc:       "MiniMax，国际站。与国内站是两套账号，密钥不通用。",
		Models:     minimaxModels(),
	},
	{
		Key: "siliconflow_cn", Name: "SiliconFlow" + cnSuffix, Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.siliconflow.cn/v1", Dialect: llm.DialectQwen,
		ConsoleURL: "https://cloud.siliconflow.cn/account/ak",
		Desc:       "硅基流动，国内站。一个密钥用多家开源模型，上下架较频繁。",
		Models:     siliconflowModels(),
	},
	{
		Key: "siliconflow", Name: "SiliconFlow", Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.siliconflow.com/v1", Dialect: llm.DialectQwen,
		ConsoleURL: "https://cloud.siliconflow.com/account/ak",
		Desc:       "硅基流动，国际站。",
		Models:     siliconflowModels(),
	},
	{
		Key: "openai", Name: "OpenAI", Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.openai.com/v1", Dialect: llm.DialectOpenAI,
		ConsoleURL: "https://platform.openai.com/api-keys",
		Desc:       "OpenAI。这一系拒绝 temperature，思考档位六档全收。",
		// 刻意不放 GPT-6：官方指南说 gpt-6-astra 与 gpt-6.1-sol 走 Chat Completions 时
		// 不能调工具，gpt-6-luna 只有关掉推理时才能调，而我们走的正是 Chat Completions
		Models: []Model{
			{ID: "gpt-5.6-terra", ContextLength: intp(1050000), MaxTokens: intp(128000)},
			{ID: "gpt-5.6-sol", ContextLength: intp(1050000), MaxTokens: intp(128000)},
			{ID: "gpt-5.6-luna", ContextLength: intp(1050000), MaxTokens: intp(128000)},
		},
	},
	{
		Key: "anthropic", Name: "Anthropic", Type: llm.TypeAnthropic,
		BaseURL:    "https://api.anthropic.com",
		ConsoleURL: "https://platform.claude.com/settings/keys",
		Desc:       "Anthropic Claude。唯一支持提示词缓存开关的一家。",
		Models: []Model{
			// 官方输出上限是 128K，这里取 64K：输出上限要从历史预算里扣掉
			{ID: "claude-sonnet-5-5", ContextLength: intp(1000000), MaxTokens: intp(64000)},
			{ID: "claude-opus-5-5", ContextLength: intp(1000000), MaxTokens: intp(64000)},
			{ID: "claude-fable-5-1", ContextLength: intp(1000000), MaxTokens: intp(64000)},
		},
	},
	{
		Key: "gemini", Name: "Google Gemini", Type: llm.TypeOpenAICompat,
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Dialect: llm.DialectGemini,
		ConsoleURL: "https://aistudio.google.com/apikey",
		Desc:       "Google Gemini 的 OpenAI 兼容端点。思考只有低中高三档。",
		Models: []Model{
			{ID: "gemini-3.8-flash", ContextLength: intp(1048576), MaxTokens: intp(65536)},
		},
	},
	{
		Key: "xai", Name: "xAI", Type: llm.TypeOpenAICompat,
		BaseURL: "https://api.x.ai/v1", Dialect: llm.DialectXAI,
		ConsoleURL: "https://console.x.ai/team/default/api-keys",
		Desc:       "xAI Grok。没有 max 档，选到会落回 xhigh。",
		Models: []Model{
			// xAI 没有公布输出上限，65536 是我们自己定的
			{ID: "grok-4.7", ContextLength: intp(500000), MaxTokens: intp(65536)},
			{ID: "grok-4.3", ContextLength: intp(1000000), MaxTokens: intp(65536)},
		},
	},
	{
		Key: "openrouter", Name: "OpenRouter", Type: llm.TypeOpenAICompat,
		BaseURL: "https://openrouter.ai/api/v1", Dialect: llm.DialectEffort,
		ConsoleURL: "https://openrouter.ai/keys",
		Desc:       "一个密钥通吃各家。带 ~ 的是始终指向该系列最新版的别名。",
		Models: []Model{
			// 能用别名就用别名：这张表本来就会过期，而别名不会。别名是按系列分的
			// （~vendor/family-latest），没有跨系列的「最新旗舰」。
			{ID: "~anthropic/claude-opus-latest"},
			{ID: "~google/gemini-flash-latest"},
			// OpenAI 这一条写死版本：它那几个系列的别名会指到 GPT-6，而 GPT-6 走
			// Chat Completions 调工具受限（见上面 OpenAI 那条）
			{ID: "openai/gpt-5.6-sol"},
		},
	},
	{
		Key: "ollama", Name: "Ollama", Type: llm.TypeOpenAICompat,
		BaseURL: "http://localhost:11434/v1", Dialect: llm.DialectNone,
		Desc: "本机推理，不需要密钥。模型要自己先 ollama pull，再在这里添加。",
		// 刻意不给模型：装了哪些只有本机知道，猜一个填进去就是个调不通的条目。
		Models: []Model{},
	},
}

func glmModels() []Model {
	return []Model{
		// glm-5.3 只接受 thinking.type=enabled，界面上调到「关闭」它会报错；
		// 钉一个 high 至少让开箱那一次是对的。
		{ID: "glm-5.3", ContextLength: intp(1000000), MaxTokens: intp(131072), Thinking: strp("high")},
		{ID: "glm-5.3-flash", ContextLength: intp(1000000), MaxTokens: intp(131072), Thinking: strp("high")},
		{ID: "glm-5.2", ContextLength: intp(1000000), MaxTokens: intp(131072)},
		{ID: "glm-4.7-flash", ContextLength: intp(200000), MaxTokens: intp(131072)},
	}
}

func kimiModels() []Model {
	return []Model{
		// 只放 K3：K2.x 的思考参数是 thinking:{type, keep}，不认 reasoning_effort，
		// 而方言是按提供商定的，同一家里分不出两种写法
		{ID: "kimi-k3", ContextLength: intp(1000000), MaxTokens: intp(65536)},
	}
}

func qwenModels() []Model {
	return []Model{
		{ID: "qwen3.7-plus", ContextLength: intp(1000000), MaxTokens: intp(65536)},
		{ID: "qwen3.8-max", ContextLength: intp(1000000), MaxTokens: intp(131072)},
		{ID: "qwen3.8-flash", ContextLength: intp(1000000), MaxTokens: intp(65536)},
	}
}

func minimaxModels() []Model {
	return []Model{
		{ID: "MiniMax-M3", ContextLength: intp(1000000), MaxTokens: intp(65536)},
		{ID: "MiniMax-M2.7", ContextLength: intp(204800), MaxTokens: intp(65536)},
		{ID: "MiniMax-M2.7-highspeed", ContextLength: intp(204800), MaxTokens: intp(65536)},
	}
}

func siliconflowModels() []Model {
	return []Model{
		{ID: "deepseek-ai/DeepSeek-V4-Flash", ContextLength: intp(1000000), MaxTokens: intp(65536)},
		{ID: "deepseek-ai/DeepSeek-V4-Pro", ContextLength: intp(1000000), MaxTokens: intp(65536)},
		// 两站共用一张清单，只放两边都有的：GLM-5.3-Flash 国内站查不到
		{ID: "zai-org/GLM-5.3", ContextLength: intp(1000000), MaxTokens: intp(131072)},
	}
}

// PresetView 是下发给界面的一条预置，字段名与 /api/models 那边的提供商对齐，
// 使前端可以直接拿它填表单。
type PresetView struct {
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	BaseURL     string  `json:"base_url"`
	Dialect     string  `json:"thinking_dialect,omitempty"`
	Models      []Model `json:"models"`
	ConsoleURL  string  `json:"console_url,omitempty"`
	Description string  `json:"description"`
	// RequiresKey 为假时界面不逼着填密钥（本机推理服务）。
	RequiresKey bool `json:"requires_key"`
}

// PresetOptions 返回按语言本地化后的预置列表。
// 说明文字经语言包取，提供商名不翻译——它是存盘后的唯一标识。
//
// existing 是当前已有的提供商，用来把重名的预置错开（DeepSeek → DeepSeek 2）。
// 错名这件事放服务端做，是因为重名的判定规则（大小写不敏感）归 validateDoc，
// 而调用方有两个——浏览器与 wen config，各自实现一遍迟早会与那条规则对不上。
func PresetOptions(lang string, existing []Provider) []PresetView {
	out := make([]PresetView, 0, len(presets))
	for _, p := range presets {
		out = append(out, PresetView{
			Key:         p.Key,
			Name:        UniqueName(existing, p.Name),
			Type:        p.Type,
			BaseURL:     p.BaseURL,
			Dialect:     p.Dialect,
			Models:      append([]Model(nil), p.Models...),
			ConsoleURL:  p.ConsoleURL,
			Description: i18n.T(lang, presetDescKey(p.Key), p.Desc),
			RequiresKey: RequiresKey(p.BaseURL),
		})
	}
	return out
}

// presetDescKey 由 Key 推出语言包的键。键是拼出来的，静态扫描看不见它，
// 因此覆盖测试改走 PresetDescs 那条推导路径（与插件元数据同一种办法）。
func presetDescKey(key string) string {
	return fmt.Sprintf("modelcfg.preset.%s.desc", key)
}

// PresetDescs 返回「语言包的键 → 中文原文」，供覆盖测试推出该有哪些译文。
func PresetDescs() map[string]string {
	out := make(map[string]string, len(presets))
	for _, p := range presets {
		out[presetDescKey(p.Key)] = p.Desc
	}
	return out
}

// UniqueName 在已有提供商里为预置名找一个不冲突的写法（DeepSeek → DeepSeek 2）。
// 放在这里而不是前端：重名的判定规则（大小写不敏感）归 validateDoc，
// 两处各写一份迟早会对不上。
func UniqueName(existing []Provider, want string) string {
	taken := make(map[string]bool, len(existing))
	for _, p := range existing {
		taken[strings.ToLower(strings.TrimSpace(p.Name))] = true
	}
	if !taken[strings.ToLower(want)] {
		return want
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s %d", want, i)
		if !taken[strings.ToLower(cand)] {
			return cand
		}
	}
}
