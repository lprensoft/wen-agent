package main

import (
	"os"
	"path/filepath"
	"testing"

	"wen/internal/config"
	"wen/internal/i18n"
)

func TestConfigArg(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"status", "-c", "a.yaml"}, "a.yaml"},
		{[]string{"status", "-c=a.yaml"}, "a.yaml"},
		{[]string{"status", "--c", "a.yaml"}, "a.yaml"},
		{[]string{"status", "--c=a.yaml"}, "a.yaml"},
		{[]string{"status"}, ""},
		{[]string{"status", "-c"}, ""}, // 末尾的 -c 后面没有值
	} {
		if got := configArg(c.args); got != c.want {
			t.Errorf("configArg(%v) = %q，期望 %q", c.args, got, c.want)
		}
	}
}

// PeekLang 的重点在「出任何差错都答空串」：它跑在参数分发之前，那时报错没有地方去，
// 而一个读不出来的语言名不该让 wen help 打不出来。
func TestPeekLang(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		name, path, want string
	}{
		{"读得出", write("a.yaml", "lang: en\nmodel:\n  provider: x\n"), "en"},
		{"没写", write("b.yaml", "model:\n  provider: x\n"), ""},
		{"文件不存在", filepath.Join(dir, "nope.yaml"), ""},
		{"不是 YAML", write("c.yaml", "\t- [[[\n"), ""},
	} {
		if got := config.PeekLang(c.path); got != c.want {
			t.Errorf("%s: PeekLang = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 配置里写了语言就以它为准；写了个不支持的按没写算，退到下一档而不是报错。
func TestResolveLangPrefersConfig(t *testing.T) {
	if got := resolveLang(&config.Config{Lang: i18n.LangEN}); got != i18n.LangEN {
		t.Errorf("配置里的语言应当优先，得到 %q", got)
	}
	if got := resolveLang(&config.Config{Lang: "kl"}); got == "kl" {
		t.Error("不支持的语言名不该被采纳")
	}
}
