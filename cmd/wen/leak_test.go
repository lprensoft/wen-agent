package main

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// keptChinese 是英文文本里有意保留的中文，逐条记下理由。它们不是漏译：
//
//   - 「雯」是角色的中文名。英文那份写成 Wen (雯)——名字本来就有两种写法，去掉它
//     模型就不知道对方用中文会怎么叫她。
//   - 「杭州」是举例。那个配置项填的是城市名，而它认中文输入，例子里得有一个中文的，
//     否则英文用户不会知道可以那么填。
//
// 收得这么窄是有意的：真正漏出来的是整句话，不会只有一个字，因此挡在这里的只可能
// 是这两个词本身。
var keptChinese = []string{"雯", "杭州"}

// hanIn 挑出一段文本里的汉字。用 unicode.Han 而不是手写码点区间：扩展区、兼容
// 表意文字都算，漏掉它们等于给漏译留了后门。
func hanIn(s string) string {
	for _, kept := range keptChinese {
		s = strings.ReplaceAll(s, kept, "")
	}
	seen := map[rune]bool{}
	var out []rune
	for _, r := range s {
		if unicode.Is(unicode.Han, r) && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return string(out)
}

// fullManager 把全部内置插件建起来并逐个打开，返回可供各处提问的 Manager。
//
// 「逐个打开」是这条测试成立的前提：默认关掉的那几个（要凭证、要触发词）的提示词
// 与 Schema 一样会发给模型，只是要用户先打开。不打开的话测试会「通过」，只是什么
// 都没查——schema_test.go 里那句 len(tools) < 20 的断言防的正是这个。
func fullManager(t *testing.T, lang string) *plugin.Manager {
	t.Helper()
	cfg := &config.Config{BaseDir: t.TempDir()}
	ictx := plugin.InitContext{
		Workdir:    t.TempDir(),
		Lang:       lang,
		SessionDir: t.TempDir(),
		Sessions:   stubSessions{},
		RunTurn:    func(context.Context, string, string) (string, error) { return "", nil },
		NewSession: func() (string, error) { return "s1", nil },
		Compact:    func(context.Context, string) error { return nil },
		Notice:     func(context.Context, string, string) error { return nil },
	}
	m := buildPlugins(cfg, ictx, nil)
	m.Resolve()
	for _, st := range m.List() {
		if !st.Enabled {
			_ = m.SetEnabled(st.Name, true)
		}
	}
	t.Cleanup(m.StopAll)
	return m
}

// TestEnglishModelContextHasNoChinese 是英文那条路的完备性检查，管的是**插件注入
// 的那半边**（核心那半边由 internal/agent 的同名检查盯）。
//
// 它与 TestPromptCoverage 查的不是一件事，两条都需要：覆盖测试逐条键去看译文有没有
// 填，这里看的是**拼出来的那份上下文**——键都填了但某一处忘了走 prompt.T，覆盖测试
// 是发现不了的，它只知道语言包里有那条键。反过来，某条键填了却没人用，也只有覆盖
// 测试查得出来。
func TestEnglishModelContextHasNoChinese(t *testing.T) {
	m := fullManager(t, i18n.LangEN)
	ctx := context.Background()
	ev := plugin.TurnEvent{SessionID: "s1", UserInput: "hello"}

	check := func(where, s string) {
		t.Helper()
		if han := hanIn(s); han != "" {
			t.Errorf("%s 里有中文 %q:\n%s", where, han, s)
		}
	}

	for _, s := range m.SystemPrompts() {
		check("常驻提示词", s)
	}
	for _, s := range m.TurnPrompts(ctx, ev) {
		check("本轮提示词", s)
	}
	for _, s := range m.CompactPrompts(ctx) {
		check("压缩提示词", s)
	}
	for _, s := range m.DayFacts(time.Now().Format("2006-01-02")) {
		check("当日事实", s)
	}

	tools := m.EnabledTools()
	if len(tools) < 20 {
		t.Fatalf("只取到 %d 个工具，多半是有一批插件没起来，这条测试就成了空跑", len(tools))
	}
	for _, tool := range tools {
		check("工具 "+tool.Name()+" 的名字", tool.Name())
		check("工具 "+tool.Name()+" 的说明", tool.Description())
		check("工具 "+tool.Name()+" 的 Schema", string(tool.Schema()))
	}
}

// TestEnglishUserFacingHasNoChinese 管用户侧：设置页那批下发的文案，以及状态命令的
// 插件状态行。判据与上面那条相同，只是语言包换成 internal/i18n。
func TestEnglishUserFacingHasNoChinese(t *testing.T) {
	m := fullManager(t, i18n.LangEN)

	for _, st := range plugin.Localize(i18n.LangEN, m.List()) {
		check := func(where, s string) {
			t.Helper()
			if han := hanIn(s); han != "" {
				t.Errorf("插件 %s 的%s里有中文 %q: %s", st.Name, where, han, s)
			}
		}
		check("说明", st.Description)
		check("分组", st.Category)
		for _, f := range st.ConfigFields {
			check("配置项 "+f.Key+" 的标签", f.Label)
			check("配置项 "+f.Key+" 的说明", f.Description)
			for _, o := range f.Options {
				check("配置项 "+f.Key+" 的候选值", o.Label)
			}
		}
		for _, a := range st.Actions {
			check("操作 "+a.Key+" 的标签", a.Label)
			check("操作 "+a.Key+" 的说明", a.Description)
		}
	}

	for _, line := range m.StatusLines(i18n.LangEN) {
		if han := hanIn(line); han != "" {
			t.Errorf("状态行里有中文 %q: %s", han, line)
		}
	}
}

// TestChinesePathUnchanged 是「中文用户感觉不到任何区别」那条要求的哨兵：中文那边
// 拼出来的提示词里不该出现英文句子。
//
// 判据只能是启发式的（提示词里本来就有 JSON 键名、工具名、块名里的英文），所以只挑
// 一种确定的错法：**连续三个及以上的英文单词**。那是句子，不是标识符。
func TestChinesePathUnchanged(t *testing.T) {
	m := fullManager(t, i18n.LangZH)
	ctx := context.Background()
	ev := plugin.TurnEvent{SessionID: "s1", UserInput: "你好"}

	var all []string
	all = append(all, m.SystemPrompts()...)
	all = append(all, m.TurnPrompts(ctx, ev)...)
	all = append(all, m.CompactPrompts(ctx)...)
	for _, tool := range m.EnabledTools() {
		all = append(all, tool.Description())
	}

	var bad []string
	for _, s := range all {
		for _, line := range strings.Split(s, "\n") {
			if run := englishRun(line); run != "" {
				bad = append(bad, run+"  ← "+line)
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("中文提示词里混进了英文句子: %s", b)
	}
}

// englishRun 找出一行里连续三个及以上的英文单词，找不到返回空串。
func englishRun(line string) string {
	fields := strings.FieldsFunc(line, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '\''
	})
	run := 0
	for _, f := range fields {
		if isWord(f) {
			run++
			if run >= 3 {
				return line
			}
			continue
		}
		run = 0
	}
	return ""
}

// isWord 判断一个词是不是纯英文单词（两个字母以上、没有下划线与数字——那些是
// 标识符，提示词里本来就有）。
func isWord(f string) bool {
	if len(f) < 2 {
		return false
	}
	for _, r := range f {
		if !unicode.IsLetter(r) || r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// TestEnglishRunDetectsSentences 盯住上面那把尺子本身。
//
// 一条永远不响的检查与没有检查是一回事，而 englishRun 恰好属于「收紧一点就会静默
// 失效」的那类：判据里但凡多一个排除条件，它就再也认不出任何东西，而测试照常通过。
func TestEnglishRunDetectsSentences(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"Conclusion first, then the reason.", true},
		{"你可以用 fetch_url 工具取网页正文。", false},
		{"[今日安排] 与 set_heartbeat_interval", false},
		{"记下来的事按 type / title 分组", false},
	} {
		if got := englishRun(c.in) != ""; got != c.want {
			t.Errorf("englishRun(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}
