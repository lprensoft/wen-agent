package main

import (
	"strings"
	"testing"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/plugin/builtin/roleplay"
	"wen/internal/stylecheck"
)

// TestActingMarkerMatchesPrompt 把「提示词让模型用哪个记号」与「检测器认哪个记号」
// 绑在一起。
//
// 这是一对必须同时改的东西：roleplay 的 [互动演绎] 告诉模型用【】还是 *…*，
// stylecheck 照着那个记号算演绎占比、并把演绎排除在「少用修饰」之外。分头改的
// 后果不会报错——占比会一直是 0，演绎的笔调会被当成话语来罚，而这两样都要等到
// 有人去看统计才发现。
//
// 放在 cmd/wen 是因为只有这里同时看得见提示词语言包与检测器；两个包互不认识，
// 也不该认识。
func TestActingMarkerMatchesPrompt(t *testing.T) {
	for _, c := range []struct {
		lang   string
		sample string // 一段用该语言记号写的演绎
		marker string // 提示词里必须出现的记号
	}{
		{i18n.LangZH, "【靠在椅背上】今天真的不行了。", "【】"},
		{i18n.LangEN, "*leans back in the chair* I'm done for today.", "*"},
	} {
		// 读真正注入出去的那一份：默认配置下 roleplay 的 system 提示词。
		// 直接查语言包只能验英文那半——中文的原文在 roleplay 包里，不导出。
		rp := roleplay.New()
		fields := plugin.ConfigFieldsOf(rp)
		cfg, err := plugin.NormalizeConfig(plugin.LocalizeDefaults(c.lang, "roleplay", fields), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := rp.Init(plugin.InitContext{Lang: c.lang}, cfg); err != nil {
			t.Fatal(err)
		}
		rules := rp.SystemPrompt()
		if !strings.Contains(rules, c.marker) {
			t.Errorf("%s: [互动演绎] 里没有出现记号 %q，检测器却按它找演绎", c.lang, c.marker)
		}
		if r := stylecheck.Measure(c.lang, c.sample).BracketRatio; r <= 0 {
			t.Errorf("%s: 检测器没把 %q 认成演绎（占比 %v）", c.lang, c.sample, r)
		}
	}
}
