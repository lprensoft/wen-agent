package weather

import (
	"testing"

	"wen/internal/i18n"
)

// 天气说法会写进观测缓存，也被 isWet 按用字判断降水。落盘的必须永远是中文规范值——
// 否则换一次语言，缓存里旧的中文与新写的英文并存，isWet 认不出后者，「刚下起雨」
// 这类开口理由就再也不会产生。
func TestConditionOfIsAlwaysCanonicalChinese(t *testing.T) {
	for code := range wmoText {
		zh := conditionOf(code)
		if conditions.For(i18n.LangZH, zh) != zh {
			t.Errorf("代码 %d 的说法 %q 不是规范值", code, zh)
		}
		// 每种语言都译得出来，才不会在注入里露出一个中文词
		if got := conditions.For(i18n.LangEN, zh); got == "" {
			t.Errorf("代码 %d 的说法 %q 没有英文译名", code, zh)
		}
	}
	if got := conditionOf(12345); got != condUnknown {
		t.Errorf("未收录的代码应回落到 %q，got %q", condUnknown, got)
	}
}

// isWet 判的是规范值，与界面语言无关。
func TestIsWetJudgesCanonicalValues(t *testing.T) {
	wet := []string{condLightRain, condHeavyRain, condFreezingRain, condLightSnow,
		condSnowShowers, condThunder, condThunderHail, condShowers}
	dry := []string{condClear, condMostlyClear, condCloudy, condOvercast, condFog, condUnknown}
	for _, c := range wet {
		if !isWet(c) {
			t.Errorf("%q 应算降水", c)
		}
	}
	for _, c := range dry {
		if isWet(c) {
			t.Errorf("%q 不该算降水", c)
		}
	}
}

// 各语言的说法必须真的不一样，否则「一条译文都没进语言包」会悄悄通过上面几条。
func TestConditionsDifferPerLanguage(t *testing.T) {
	if conditions.For(i18n.LangEN, condLightRain) == condLightRain {
		t.Error("英文的「小雨」仍是中文，说明译文没进语言包")
	}
}
