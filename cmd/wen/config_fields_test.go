package main

import (
	"reflect"
	"testing"

	"wen/internal/config"
	"wen/internal/plugin"
)

// 这条测试钉住各条 IM 通道的配置项序列、标签与默认值。
//
// 阶段 0 把重复的字段声明收进了 imbot，并承诺设置页行为不变；字段顺序、文案
// 与默认值正是这种重构最容易漂的地方，而编译器不会替我们盯着。
type fieldExpect struct {
	label string
	def   any
}

func TestIMChannelConfigFieldsGolden(t *testing.T) {
	plugins := builtinPlugins("zh", &config.Config{BaseDir: t.TempDir()}, nil)
	byName := map[string]plugin.Plugin{}
	for _, p := range plugins {
		byName[p.Name()] = p
	}

	common := map[string]fieldExpect{
		"confirm_timeout_sec": {"确认等待超时（秒）", 300},
		"show_thinking":       {"展示思考过程", false},
		"show_tools":          {"展示工具调用", false},
		"push_notices":        {"推送后台通知", false},
		"merge_window_sec":    {"合并连发消息（秒）", 3},
		"human_pace":          {"像人一样发消息", false},
		"busy_hold":           {"忙时晚点回", false},
		"wake_after_minutes":  {"睡着时多久被吵醒（分钟）", 15},
	}
	withCommon := func(extra map[string]fieldExpect) map[string]fieldExpect {
		m := map[string]fieldExpect{}
		for k, v := range common {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	cases := []struct {
		name  string
		order []string
		want  map[string]fieldExpect
	}{
		{
			name:  "qq_bot",
			order: []string{"app_id", "app_secret", "whitelist", "api_base", "confirm_timeout_sec", "show_thinking", "show_tools", "push_notices", "format", "merge_window_sec", "human_pace", "busy_hold", "wake_after_minutes"},
			want: withCommon(map[string]fieldExpect{
				"app_id":     {"AppID", ""},
				"app_secret": {"AppSecret", ""},
				"whitelist":  {"openid 白名单", ""},
				"api_base":   {"API 地址", "https://api.bot.qq.com"},
				"format":     {"消息格式", "markdown"},
			}),
		},
		{
			name:  "feishu_bot",
			order: []string{"app_id", "app_secret", "whitelist", "api_base", "confirm_timeout_sec", "format", "show_thinking", "show_tools", "push_notices", "merge_window_sec", "human_pace", "busy_hold", "wake_after_minutes"},
			want: withCommon(map[string]fieldExpect{
				"app_id":     {"App ID", ""},
				"app_secret": {"App Secret", ""},
				"whitelist":  {"open_id 白名单", ""},
				"api_base":   {"API 地址", "https://open.feishu.cn"},
				"format":     {"消息格式", "card"},
			}),
		},
		{
			name:  "lark_bot",
			order: []string{"app_id", "app_secret", "whitelist", "api_base", "confirm_timeout_sec", "format", "show_thinking", "show_tools", "push_notices", "merge_window_sec", "human_pace", "busy_hold", "wake_after_minutes"},
			want: withCommon(map[string]fieldExpect{
				"app_id":     {"App ID", ""},
				"app_secret": {"App Secret", ""},
				"whitelist":  {"open_id 白名单", ""},
				"api_base":   {"API 地址", "https://open.larksuite.com"},
				"format":     {"消息格式", "card"},
			}),
		},
		{
			name:  "telegram_bot",
			order: []string{"bot_token", "whitelist", "proxy", "api_base", "confirm_timeout_sec", "format", "stream_preview", "show_thinking", "show_tools", "push_notices", "merge_window_sec", "human_pace", "busy_hold", "wake_after_minutes"},
			want: withCommon(map[string]fieldExpect{
				"bot_token":      {"Bot Token", ""},
				"whitelist":      {"chat_id 白名单", ""},
				"proxy":          {"代理地址", ""},
				"api_base":       {"API 地址", "https://api.telegram.org"},
				"format":         {"消息格式", "markdown"},
				"stream_preview": {"边写边显示", false},
			}),
		},
		{
			name:  "wechat_bot",
			order: []string{"whitelist", "api_base", "confirm_timeout_sec", "format", "show_thinking", "show_tools", "push_notices", "merge_window_sec", "human_pace", "busy_hold", "wake_after_minutes"},
			want: withCommon(map[string]fieldExpect{
				"whitelist": {"用户白名单", ""},
				"api_base":  {"API 地址", "https://ilinkai.weixin.qq.com"},
				"format":    {"消息格式", "markdown"},
			}),
		},
		{
			name:  "discord_bot",
			order: []string{"bot_token", "whitelist", "proxy", "api_base", "gateway_url", "confirm_timeout_sec", "format", "show_thinking", "show_tools", "push_notices", "merge_window_sec", "human_pace", "busy_hold", "wake_after_minutes"},
			want: withCommon(map[string]fieldExpect{
				"bot_token":   {"Bot Token", ""},
				"whitelist":   {"DM channel 白名单", ""},
				"proxy":       {"代理地址", ""},
				"api_base":    {"API 地址", "https://discord.com/api/v10"},
				"gateway_url": {"Gateway 地址", "wss://gateway.discord.gg/?v=10&encoding=json"},
				"format":      {"消息格式", "markdown"},
			}),
		},
		{
			name:  "wecom_bot",
			order: []string{"bot_id", "secret", "whitelist", "gateway_url", "confirm_timeout_sec", "format", "show_thinking", "show_tools", "push_notices", "merge_window_sec", "human_pace", "busy_hold", "wake_after_minutes"},
			want: withCommon(map[string]fieldExpect{
				"bot_id":      {"BotID", ""},
				"secret":      {"Secret", ""},
				"whitelist":   {"userid 白名单", ""},
				"gateway_url": {"长连接地址", "wss://openws.work.weixin.qq.com"},
				"format":      {"消息格式", "markdown"},
			}),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := byName[c.name]
			if p == nil {
				t.Fatalf("找不到插件 %s", c.name)
			}
			configurable, ok := p.(plugin.Configurable)
			if !ok {
				t.Fatalf("%s 没有实现 Configurable", c.name)
			}
			fields := configurable.ConfigFields()
			var keys []string
			for _, f := range fields {
				keys = append(keys, f.Key)
			}
			if !reflect.DeepEqual(keys, c.order) {
				t.Fatalf("配置项顺序变了:\n got %v\nwant %v", keys, c.order)
			}
			for _, f := range fields {
				w, ok := c.want[f.Key]
				if !ok {
					t.Errorf("字段 %q 没有期望值", f.Key)
					continue
				}
				if f.Label != w.label {
					t.Errorf("%s 标签 = %q，想要 %q", f.Key, f.Label, w.label)
				}
				if !reflect.DeepEqual(f.Default, w.def) {
					t.Errorf("%s 默认值 = %#v，想要 %#v", f.Key, f.Default, w.def)
				}
			}
			if len(fields) != len(c.order) {
				t.Errorf("字段数 = %d，想要 %d", len(fields), len(c.order))
			}
		})
	}
}
