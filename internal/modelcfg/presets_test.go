package modelcfg

import (
	"strings"
	"testing"

	"wen/internal/llm"
)

// 预置表是手写的一张常量表，最容易出的错不是逻辑而是笔误：地址少个 /v1、
// 方言名拼错、两条重名。这些错误只有在用户挑中那一家、填好密钥、发出第一条
// 消息之后才会暴露——那时他多半会以为是自己的密钥有问题。
// 所以让整张表走一遍与用户配置同一套校验。
func TestPresetsPassValidation(t *testing.T) {
	for _, p := range presets {
		doc := File{Version: 1, Providers: []Provider{{
			Name: p.Name, Type: p.Type, BaseURL: p.BaseURL,
			Dialect: p.Dialect, Models: p.Models,
		}}}
		if err := validateDoc("", doc); err != nil {
			t.Errorf("预置 %s 过不了校验: %v", p.Key, err)
		}
	}
}

func TestPresetKeysAndNamesUnique(t *testing.T) {
	keys, names := map[string]bool{}, map[string]bool{}
	for _, p := range presets {
		if keys[p.Key] {
			t.Errorf("预置 Key %q 重复", p.Key)
		}
		keys[p.Key] = true
		// 名字大小写不敏感地唯一，与 validateDoc 的重名判定一致
		lower := strings.ToLower(p.Name)
		if names[lower] {
			t.Errorf("预置名 %q 重复", p.Name)
		}
		names[lower] = true
	}
}

func TestPresetFieldsAreSane(t *testing.T) {
	for _, p := range presets {
		if p.Key == "" || p.Name == "" || p.Desc == "" {
			t.Errorf("预置 %q 缺 Key / Name / Desc", p.Key)
		}
		// 方言只对 openai_compat 有意义；anthropic 那边留空
		if p.Type == llm.TypeAnthropic && p.Dialect != "" {
			t.Errorf("预置 %s 是 anthropic 模式，不该带思考参数方言 %q", p.Key, p.Dialect)
		}
		if p.Type == llm.TypeOpenAICompat && p.Dialect == "" {
			t.Errorf("预置 %s 没写方言——空串会落到 deepseek，这该是个明确的选择", p.Key)
		}
		// 我们直接拼 /chat/completions，路径少一截就是个 404，而报错里只有状态码
		if p.Type == llm.TypeOpenAICompat && strings.HasSuffix(p.BaseURL, "/") {
			t.Errorf("预置 %s 的 base_url 末尾多一个斜杠: %s", p.Key, p.BaseURL)
		}
		// 除本机推理外都得有模型，否则挑中之后还是切不过去
		if len(p.Models) == 0 && RequiresKey(p.BaseURL) {
			t.Errorf("预置 %s 没有模型，用户挑中它之后仍然用不了", p.Key)
		}
		if p.ConsoleURL == "" && RequiresKey(p.BaseURL) {
			t.Errorf("预置 %s 要密钥却没说去哪儿拿", p.Key)
		}
	}
}

// 预置只是填表单的初值，不是第三种配置来源：挑中一条之后存下来的就是一条
// 普通的用户条目。这里盯的是「拿来即可保存」——PresetOptions 给出的东西
// 原样提交必须能过 Save。
func TestPresetOptionsAreSaveable(t *testing.T) {
	s, _ := newStore(t, baseConfig())
	for _, v := range PresetOptions("", s.Snapshot().Providers) {
		next := s.Snapshot()
		next.Providers = append(next.Providers, Provider{
			Name: v.Name, Type: v.Type, BaseURL: v.BaseURL,
			Dialect: v.Dialect, APIKey: "123", Models: v.Models,
		})
		if _, err := s.Save("", next); err != nil {
			t.Fatalf("预置 %s 存不进去: %v", v.Key, err)
		}
	}
}

// 重名要错开，否则挑第二次同一家时保存会被 validateDoc 顶回来，
// 而那个报错说的是「名称重复」，看起来像用户自己填错了。
func TestPresetNameAvoidsExisting(t *testing.T) {
	existing := []Provider{{Name: "deepseek"}, {Name: "DeepSeek 2"}}
	if got := UniqueName(existing, "DeepSeek"); got != "DeepSeek 3" {
		t.Errorf("UniqueName = %q，想要 DeepSeek 3", got)
	}
	if got := UniqueName(nil, "DeepSeek"); got != "DeepSeek" {
		t.Errorf("没有冲突时不该改名，得到 %q", got)
	}
}

func TestRequiresKey(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:11434/v1":   false,
		"http://127.0.0.1:1234/v1":    false,
		"https://api.deepseek.com":    true,
		"https://api.openai.com/v1":   true,
		"http://192.168.1.9:11434/v1": true, // 局域网里的另一台机器不算本机
	}
	for url, want := range cases {
		if got := RequiresKey(url); got != want {
			t.Errorf("RequiresKey(%q) = %v，想要 %v", url, got, want)
		}
	}
}
