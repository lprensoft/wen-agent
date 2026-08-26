package memory

import (
	"strings"
	"testing"

	"wen/internal/i18n"
)

// 分类会写进每条记忆的 frontmatter，也出现在「分类/标题」这个串里。落盘的必须
// 永远是中文规范值——否则换一次语言，同一个字段在磁盘上就有了两种写法，索引分组、
// 「边界类不淡忘」那条判定、按分类过滤全部对不上。
func TestTypeIsStoredAsChineseInEveryLanguage(t *testing.T) {
	for _, l := range i18n.Languages() {
		s := NewStore(l.Code, t.TempDir())
		for _, zh := range Types {
			local := memTypes.For(l.Code, zh)
			e, err := s.Save(Entry{
				Name:    "条目-" + l.Code + "-" + zh,
				Type:    local,
				Content: "正文",
			}, false)
			if err != nil {
				t.Fatalf("%s 下存 %q 失败：%v", l.Code, local, err)
			}
			if e.Type != zh {
				t.Errorf("%s 下存 %q，落盘为 %q，want %q", l.Code, local, e.Type, zh)
			}
		}
	}
}

// 「分类/标题」的前缀要认任意语言：索引按界面语言渲染，模型照着它回传。
func TestFindEntryStripsTypePrefixInEveryLanguage(t *testing.T) {
	entries := []Entry{{Name: "喜欢香菜", Slug: "xihuan", Type: "偏好"}}
	for _, l := range i18n.Languages() {
		name := memTypes.For(l.Code, "偏好") + "/喜欢香菜"
		if _, ok := findEntry(entries, name); !ok {
			t.Errorf("%s 下 %q 应能定位到那条记忆", l.Code, name)
		}
	}
}

// 分类不合法时报错而不是悄悄归到默认类：存错分类的记忆会一直待在错误的分组里。
func TestUnknownTypeIsRejected(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, err := s.Save(Entry{Name: "x", Type: "随便什么", Content: "正文"}, false); err == nil {
		t.Fatal("未知分类应报错")
	}
}

// 各语言的候选项必须真的不一样，否则「六个键都没译」会悄悄通过上面几条。
func TestTypeValuesDifferPerLanguage(t *testing.T) {
	zh := strings.Join(memTypes.Values(i18n.LangZH), "|")
	en := strings.Join(memTypes.Values(i18n.LangEN), "|")
	if zh == en {
		t.Errorf("中英的分类候选项相同（%s），说明译文没进语言包", zh)
	}
}
