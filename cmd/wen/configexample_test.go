package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 两份样例配置：中文是主版本，英文跟着它走。
const (
	exampleZH = "config.example.yaml"
	exampleEN = "config.example.en.yaml"
)

// commentedOption 认出「被注释掉的配置项」那种注释行（`# lang: zh`、`#   type: anthropic`），
// 把它与纯说明文字区分开。
//
// 判据收得紧，为的是不把散文误认成配置：键必须是小写标识符，冒号必须是半角的。
// 英文里的 `# Note: ...` 因为首字母大写而落选，中文里的「加载优先级：」用的是全角冒号。
var commentedOption = regexp.MustCompile(`^(\s*)#\s*([a-z_][a-z0-9_]*):`)

// inlineComment 找出行尾注释的起点：一个前面有空白的 #。
// 值里真要出现 # 就得加引号，那时它不在行首空白之后，认不着。
var inlineComment = regexp.MustCompile(`\s+#`)

// skeleton 是一份样例配置去掉说明文字之后剩下的东西：真正生效的行原样保留，
// 被注释掉的选项只留缩进与键名。两份样例的这部分必须完全一致。
type skeleton struct {
	live []string // 生效的配置行（已去掉行尾注释）
	opts []string // 被注释掉的配置项，形如 "  trust_loopback"
}

func readSkeleton(t *testing.T, name string) skeleton {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, name))
	if err != nil {
		t.Fatalf("读 %s: %v", name, err)
	}
	var sk skeleton
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			if m := commentedOption.FindStringSubmatch(line); m != nil {
				sk.opts = append(sk.opts, m[1]+m[2])
			}
			continue
		}
		if loc := inlineComment.FindStringIndex(line); loc != nil {
			line = line[:loc[0]]
		}
		sk.live = append(sk.live, strings.TrimRight(line, " \t"))
	}
	return sk
}

// TestConfigExamplesMatch 盯住两份样例配置的骨架。
//
// 说明文字必须能各写各的——英文比中文长，硬要求行数对齐等于绑住译者的手。但**配置
// 本身**不许分叉：加了一项只加在中文那份里，英文用户照着复制就少一项，而这既不会
// 报错也没人看得出来。文档那边的中英两版只能靠人记着同步（散文没法比对），配置样例
// 是结构化的，所以这里能把「记得同步」换成「不同步就红」。
//
// 被注释掉的那些选项一并比：`lang:` 本身就是注释掉的，只比生效的行等于漏掉了它们。
func TestConfigExamplesMatch(t *testing.T) {
	zh, en := readSkeleton(t, exampleZH), readSkeleton(t, exampleEN)

	diff := func(what string, a, b []string) {
		t.Helper()
		if strings.Join(a, "\n") == strings.Join(b, "\n") {
			return
		}
		t.Errorf("两份样例配置的%s对不上\n%s:\n%s\n\n%s:\n%s",
			what, exampleZH, strings.Join(a, "\n"), exampleEN, strings.Join(b, "\n"))
	}
	diff("配置行", zh.live, en.live)
	diff("注释掉的配置项", zh.opts, en.opts)

	if len(zh.live) < 10 || len(zh.opts) < 3 {
		t.Fatalf("只抽出 %d 行配置与 %d 个注释掉的选项，多半是解析出了问题，这条测试就成了空跑",
			len(zh.live), len(zh.opts))
	}
}

// 英文那份里不该有中文——它存在的全部理由就是给读不懂中文的人看。
func TestEnglishExampleHasNoChinese(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, exampleEN))
	if err != nil {
		t.Fatal(err)
	}
	// 首行的语言互链除外：那一行指向中文版，写的就是「中文」两个字
	body := strings.SplitN(string(raw), "\n", 2)[1]
	if han := hanIn(body); han != "" {
		t.Errorf("%s 里有中文 %q", exampleEN, han)
	}
}
