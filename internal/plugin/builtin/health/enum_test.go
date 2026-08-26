package health

import (
	"testing"
	"time"

	"wen/internal/i18n"
)

// 处理方式是本插件唯一会**原样落盘**的枚举。这条盯住往返：不论界面是哪门语言，
// 存进 Condition.Care 的都必须是中文规范值——否则换一次语言，同一个字段就有了两种
// 写法，而磁盘上已经躺着一批旧的，careFactor 那几处的比较会静默地全部落到默认分支。
func TestCareIsStoredAsChineseInEveryLanguage(t *testing.T) {
	for _, l := range i18n.Languages() {
		for i, v := range careKinds.Values(l.Code) {
			s := NewStore(l.Code, t.TempDir())
			now := time.Now()
			c, err := s.Add("着凉", severities.Values(l.Code)[0], now, 3, v, now, Limits{})
			if err != nil {
				t.Fatalf("%s: Add(care=%q): %v", l.Code, v, err)
			}
			if want := careKinds.Values(i18n.LangZH)[i]; c.Care != want {
				t.Errorf("%s: 报 %q 存下了 %q，期望中文规范值 %q", l.Code, v, c.Care, want)
			}
		}
	}
}

// 严重度不落盘（写入时换算成 Peak 数值），但同样要认所有语言的字面量。
func TestSeverityAcceptsEveryLanguage(t *testing.T) {
	for _, l := range i18n.Languages() {
		for _, v := range severities.Values(l.Code) {
			if _, ok := peakFor(v); !ok {
				t.Errorf("%s: 严重度 %q 不被接受", l.Code, v)
			}
		}
	}
}
