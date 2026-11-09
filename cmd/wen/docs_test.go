package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/config"
	"wen/internal/i18n"
)

// TestPluginDocsListEveryBuiltin 盯住插件总览表：新增内置插件时忘了写文档，
// 中文或英文那一份就会漏掉它，而语言包覆盖测试只看得见文案、看不见插件清单。
func TestPluginDocsListEveryBuiltin(t *testing.T) {
	plugins := builtinPlugins(i18n.LangZH, &config.Config{}, nil)
	if len(plugins) == 0 {
		t.Fatal("no built-in plugins were constructed")
	}

	docs := []string{
		"docs/plugins/README.md",
		"docs/en/plugins/README.md",
	}
	text := map[string]string{}
	for _, rel := range docs {
		raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text[rel] = string(raw)
	}

	for _, p := range plugins {
		name := p.Name()
		for _, rel := range docs {
			if !strings.Contains(text[rel], "`"+name+"`") {
				t.Errorf("%s does not list plugin %q", rel, name)
			}
		}
	}
}
