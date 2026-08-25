package main

import (
	"os"
	"sort"
	"testing"

	"wen/internal/i18n"
	"wen/internal/prompt"
)

// TestPromptCoverage 盯住模型侧的语言包。
//
// 与 TestLocaleCoverage 是同一件事的另一半，但后果不同：界面漏一条译文只是英文用户
// 面前冒出一行中文，模型漏一条译文则是它收到一份半中半英的指令——同一个块名在一处
// 是中文、另一处是英文时，模型不会报错，只会照着一个空指代往下演。
//
// 导出待填的骨架：
//
//	WEN_PROMPT_DUMP=missing WEN_PROMPT_OUT=<路径> go test ./cmd/wen -run TestPromptCoverage
func TestPromptCoverage(t *testing.T) {
	if err := prompt.Err(); err != nil {
		t.Fatalf("提示词语言包加载失败: %v", err)
	}
	want := map[string]string{}
	for k, site := range scanRepo(t).Prompt {
		want[k] = site.ZH
	}

	switch os.Getenv("WEN_PROMPT_DUMP") {
	case "zh":
		dumpTo(t, want, "WEN_PROMPT_OUT")
		return
	case "missing":
		have := keySet(prompt.Keys(i18n.LangEN))
		missing := map[string]string{}
		for k, zh := range want {
			if !have[k] {
				missing[k] = zh
			}
		}
		dumpTo(t, missing, "WEN_PROMPT_OUT")
		return
	}

	have := keySet(prompt.Keys(i18n.LangEN))
	var missing []string
	for k := range want {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("提示词语言包 en 缺 %d 条译文（用 WEN_PROMPT_DUMP=missing 导出待填的骨架）:\n%s",
			len(missing), joinLimit(missing, 20))
	}

	// 反向也要查：改了键之后，语言包里那条就成了死键。它不会造成故障，但会让
	// 「还差多少条」这个数字永远对不上，而这个数字正是这项工作唯一的进度指示。
	var stale []string
	for k := range have {
		if _, ok := want[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("提示词语言包 en 有 %d 条对不上任何调用点的死键:\n%s", len(stale), joinLimit(stale, 20))
	}
}
