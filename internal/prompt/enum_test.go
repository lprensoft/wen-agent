package prompt

import "testing"

var testCare = NewEnum(
	Text("prompt.health.care.tough", "硬扛"),
	Text("prompt.health.care.meds", "吃药"),
	Text("prompt.health.care.doctor", "就医"),
)

// 落盘的必须是中文规范值：英文界面下模型报的是英文字面量，存进去的仍要是中文，
// 否则同一个字段会有两种写法，而磁盘上已经躺着一批旧的。
func TestEnumCanonicalizesEveryLanguageBackToChinese(t *testing.T) {
	for _, lang := range []string{"", "zh", "en"} {
		for i, v := range testCare.Values(lang) {
			zh, ok := testCare.Canonical(v)
			if !ok {
				t.Errorf("%s 的第 %d 个候选值 %q 换不回中文", lang, i, v)
				continue
			}
			if want := testCare.Values("zh")[i]; zh != want {
				t.Errorf("%s 的 %q 换回了 %q，期望 %q", lang, v, zh, want)
			}
		}
	}
}

// 不在表里的值必须明确失败。「猜一个最接近的」会把一次参数错误变成一条悄悄存错的记录。
func TestEnumRejectsUnknown(t *testing.T) {
	if zh, ok := testCare.Canonical("躺平"); ok {
		t.Errorf("表外的值不该被认下来，得到 %q", zh)
	}
	if testCare.Has("躺平") {
		t.Error("Has 不该认表外的值")
	}
}

// 英文那份要真的是英文——三个键都漏译的话上面两条仍会通过（各语言都取到中文），
// 而模型收到的 Schema 就是一份中文候选项。
func TestEnumValuesDifferPerLanguage(t *testing.T) {
	zh, en := testCare.Values("zh"), testCare.Values("en")
	for i := range zh {
		if zh[i] == en[i] {
			t.Errorf("第 %d 个候选值中英相同（%q），多半是漏译", i, zh[i])
		}
	}
}

// JSON 拼出来的要能直接进 Schema。
func TestEnumJSON(t *testing.T) {
	if got := testCare.JSON("zh"); got != `["硬扛","吃药","就医"]` {
		t.Errorf("JSON(zh) = %s", got)
	}
	if got := NewEnum().JSON("en"); got != "[]" {
		t.Errorf("空枚举应给出 []，得到 %s", got)
	}
}
