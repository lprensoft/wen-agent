package modelcfg

import (
	"testing"

	"wen/internal/config"
)

// 提示词缓存的开关有钱的含义（命中约十分之一价、写入多付约四分之一），
// 因此三态必须分得清：未设置=开启，明确关掉才是关。
func TestResolvePromptCacheDefaultsOn(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name string
		set  *bool
		want bool
	}{
		{"未设置按开启算", nil, true},
		{"明确开启", &on, true},
		{"明确关闭", &off, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Store{
				own: File{
					Providers: []Provider{{
						Name: "ant", Type: "anthropic", BaseURL: "https://api.anthropic.com",
						APIKey: "123", PromptCache: c.set, Models: []Model{{ID: "m"}},
					}},
					Current: Selection{Provider: "ant", Model: "m"},
				},
				defaults: config.ModelConfig{MaxTokens: 4096, Thinking: "high", ContextLength: 1000},
			}
			got, err := s.Resolve("")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.PromptCache != c.want {
				t.Errorf("PromptCache = %v，期望 %v", got.PromptCache, c.want)
			}
		})
	}
}

// config.yaml 里写的开关要能传到解析结果，否则界面之外没法配置。
func TestPromptCacheFromConfigYAML(t *testing.T) {
	off := false
	cfg := &config.Config{
		Model: config.ModelConfig{Provider: "ant", Name: "m", MaxTokens: 4096, ContextLength: 1000},
		Providers: map[string]config.ProviderConfig{
			"ant": {Type: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "123", PromptCache: &off},
		},
	}
	ps := baseProviders(cfg)
	if len(ps) != 1 || ps[0].PromptCache == nil || *ps[0].PromptCache {
		t.Fatalf("config.yaml 的开关没传过来: %+v", ps)
	}
}
