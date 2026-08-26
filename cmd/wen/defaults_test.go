package main

import (
	"strings"
	"testing"

	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// defaultStates 返回全新安装（还没有 plugins.state.json）时各插件的开关。
func defaultStates(t *testing.T, lang string) map[string]plugin.Status {
	t.Helper()
	cfg := &config.Config{BaseDir: t.TempDir()}
	ictx := plugin.InitContext{Workdir: t.TempDir(), Lang: lang}
	m := buildPlugins(cfg, ictx, nil, plugin.WithoutInit())
	m.Resolve()

	out := map[string]plugin.Status{}
	for _, st := range m.List() {
		out[st.Name] = st
	}
	return out
}

// TestDefaultsShowWhatThisProjectIs 盯住开箱形态。
//
// 这不是一条风格偏好，是这个项目的门面：角色演绎那一组曾经整组默认关闭（因为它们
// 依赖同样默认关闭的 roleplay），于是装完拿到的是一个普通通用 Agent，README 上写的
// 「活人感」与「性格稳定」一个字都演示不出来。谁把 roleplay 挪回默认关闭，或者把
// 默认角色设定清空，这条就会响。
func TestDefaultsShowWhatThisProjectIs(t *testing.T) {
	states := defaultStates(t, i18n.LangZH)

	// 角色演绎组 + 它依赖的记忆与检索，一个都不能少
	for _, name := range []string{
		"roleplay", "scene", "belongings", "body_sense", "health", "mood",
		"people", "agenda", "relationship", "unspoken", "presence", "style_watch",
		"memory", "session_search", "heartbeat",
	} {
		st, ok := states[name]
		if !ok {
			t.Fatalf("插件 %q 不存在", name)
		}
		if !st.Enabled {
			t.Errorf("插件 %q 应当默认启用：开箱要能演示出角色的连续性", name)
		}
	}

	// 能读任意文件、执行任意命令的两个默认关闭——它们还能绕过可见域直接读到
	// 会话文件与记忆目录（dual_persona 正为此把它们声明为冲突）
	for _, name := range []string{"read_file", "exec_command"} {
		if st := states[name]; st.Enabled {
			t.Errorf("插件 %q 应当默认关闭：权限过大的东西要用户自己去开", name)
		}
	}
}

// TestDefaultPersonaIsUsableInEveryLanguage 盯住「没存过值时实际生效的那一份」。
//
// 默认角色设定同时活在两个地方：Go 里的中文常量，与语言包里的译文。生效的那一份由
// Manager 在 Init 之前按进程语言解析（见 Manager.fieldsFor）——这条测试确认它真的
// 解析了，而不是在英文进程里仍然把中文常量交给模型。
func TestDefaultPersonaIsUsableInEveryLanguage(t *testing.T) {
	zh, _ := defaultStates(t, i18n.LangZH)["roleplay"].Config["persona"].(string)
	if strings.TrimSpace(zh) == "" {
		t.Fatal("中文的默认角色设定为空，roleplay 开着也只是个空壳")
	}
	for _, l := range i18n.Languages() {
		st := defaultStates(t, l.Code)["roleplay"]
		persona, _ := st.Config["persona"].(string)
		if strings.TrimSpace(persona) == "" {
			t.Fatalf("%s: 默认角色设定为空", l.Code)
		}
		if !strings.Contains(persona, "Wen") && !strings.Contains(persona, "雯") {
			t.Errorf("%s: 默认角色设定里没有角色的名字:\n%s", l.Code, persona)
		}
		// 比对中文原文而不是扫中文字符：英文那份里留着「Wen（雯）」这样的名字注解
		// 是有意的，扫字符会把它误判成漏译
		if l.Code != i18n.LangZH && persona == zh {
			t.Errorf("%s: 生效的仍是中文常量，英文用户会拿到一个说中文的人格", l.Code)
		}
	}
}
