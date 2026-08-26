package agenda

import (
	"context"
	"testing"
	"time"

	"wen/internal/availability"
	"wen/internal/i18n"
)

// flex 与 busy 都会**原样落盘**。这条盯住往返：不论界面是哪门语言，存进 Item 的都
// 必须是中文规范值，否则表排好之后换一门语言，跳过判定、可挪判定与 availability
// 的档位换算会静默地全部落到默认分支。
func TestPlanEnumsAreStoredAsChineseInEveryLanguage(t *testing.T) {
	zhFlex := flexLevels.Values(i18n.LangZH)
	zhBusy := busyLevels.Values(i18n.LangZH)
	for _, l := range i18n.Languages() {
		p := &Plugin{}
		for i, f := range flexLevels.Values(l.Code) {
			it, err := p.buildItem(context.Background(), l.Code, planItemArg{
				Title: "晨跑", Start: "07:00", End: "07:40",
				Flex: f, Busy: busyLevels.Values(l.Code)[i%len(zhBusy)],
			})
			if err != nil {
				t.Fatalf("%s: buildItem(flex=%q): %v", l.Code, f, err)
			}
			if it.Flex != zhFlex[i] {
				t.Errorf("%s: flex 报 %q 存下了 %q，期望 %q", l.Code, f, it.Flex, zhFlex[i])
			}
			if want := zhBusy[i%len(zhBusy)]; it.Busy != want {
				t.Errorf("%s: busy 存下了 %q，期望 %q", l.Code, it.Busy, want)
			}
		}
	}
}

// 表上落盘的 busy 必须是 availability 认得的档位——它按中文规范值分档。
func TestStoredBusyParsesAsAvailabilityLevel(t *testing.T) {
	for _, v := range busyLevels.Values(i18n.LangZH) {
		if _, ok := availability.ParseLevel(v); !ok {
			t.Errorf("忙碌档位 %q 解析不出来", v)
		}
	}
}

// 星期名逐语言都要给满七个，缺一个就会回落到 Go 的英文星期名，注入里中英混排。
func TestWeekdayNamesCoverSevenDays(t *testing.T) {
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.Local) // 周日
	for _, l := range i18n.Languages() {
		seen := map[string]bool{}
		for i := 0; i < 7; i++ {
			n := weekdayName(l.Code, base.AddDate(0, 0, i))
			if n == "" || seen[n] {
				t.Errorf("%s: 第 %d 天的星期名 = %q（空或重复）", l.Code, i, n)
			}
			seen[n] = true
		}
	}
}
