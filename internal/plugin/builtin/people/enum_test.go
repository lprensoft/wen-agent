package people

import (
	"testing"
	"time"

	"wen/internal/i18n"
)

// 亲近度会**原样落盘**。这条盯住往返：不论界面是哪门语言，存进 Person.Closeness 的
// 都必须是中文规范值，否则换一次语言，同一个字段就有了两种写法，而磁盘上已经躺着
// 一批旧的。
func TestClosenessIsStoredAsChineseInEveryLanguage(t *testing.T) {
	for _, l := range i18n.Languages() {
		for i, v := range closenessLevels.Values(l.Code) {
			s := NewStore(l.Code, t.TempDir())
			rel, cl := "同事", v
			if _, err := s.Upsert(Update{Name: "林舟", Relation: &rel, Closeness: &cl},
				time.Now(), Limits{}); err != nil {
				t.Fatalf("%s: Upsert(closeness=%q): %v", l.Code, v, err)
			}
			ps, err := s.List()
			if err != nil || len(ps) != 1 {
				t.Fatalf("%s: List: %v", l.Code, err)
			}
			if want := closenessLevels.Values(i18n.LangZH)[i]; ps[0].Closeness != want {
				t.Errorf("%s: 报 %q 存下了 %q，期望中文规范值 %q", l.Code, v, ps[0].Closeness, want)
			}
		}
	}
}

// 同一档亲近度换个语言报一遍，不该被当成一次变化——否则英文界面下每轮都会多出
// 一句「亲近度 熟→close」。
func TestClosenessUnchangedAcrossLanguages(t *testing.T) {
	s := NewStore(i18n.LangEN, t.TempDir())
	rel, zh := "同事", closenessLevels.Values(i18n.LangZH)[2]
	if _, err := s.Upsert(Update{Name: "林舟", Relation: &rel, Closeness: &zh}, time.Now(), Limits{}); err != nil {
		t.Fatal(err)
	}
	en := closenessLevels.Values(i18n.LangEN)[2]
	res, err := s.Upsert(Update{Name: "林舟", Closeness: &en}, time.Now(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 {
		t.Errorf("同一档换个语言报，不该记成变化：%v", res.Changes)
	}
}

// 指代对方本人的称呼在任何语言下都要拦住：对方不进人物库。
func TestSelfNamesRejectedInEveryLanguage(t *testing.T) {
	for _, l := range i18n.Languages() {
		for _, v := range selfNames.Values(l.Code) {
			if _, err := normalizeName(l.Code, v); err == nil {
				t.Errorf("%s: %q 应当被拒绝", l.Code, v)
			}
		}
	}
}
