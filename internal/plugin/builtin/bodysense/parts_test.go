package bodysense

import (
	"strings"
	"testing"
)

func TestParsePartsDefaults(t *testing.T) {
	parts, err := parseParts("", defaultPartsDaily, defaultPartsClose, defaultPartsIntimate, defaultPartsPrivate)
	if err != nil {
		t.Fatalf("默认部位表应当合法: %v", err)
	}
	if len(parts) != 27 {
		t.Fatalf("默认部位表条数 = %d，期望 27", len(parts))
	}
	// 顺序必须是「日常 → 私密」的梯度，注入与 enum 都靠它
	if parts[0].Name != "手" || parts[0].Privacy != privacyDaily {
		t.Errorf("首条 = %+v，期望「手」且为日常档", parts[0])
	}
	if last := parts[len(parts)-1]; last.Privacy != privacyPrivate {
		t.Errorf("末条 = %+v，期望落在私密档", last)
	}
	for i := 1; i < len(parts); i++ {
		if parts[i].Privacy < parts[i-1].Privacy {
			t.Fatalf("第 %d 条的私密度回退了：%+v → %+v", i, parts[i-1], parts[i])
		}
	}
}

func TestParsePartsSkipsBlankLines(t *testing.T) {
	parts, err := parseParts("", "手\n\n  \n肩\r\n", "", "", "")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(parts) != 2 || parts[0].Name != "手" || parts[1].Name != "肩" {
		t.Fatalf("解析结果 = %+v，期望「手」「肩」两条", parts)
	}
}

func TestParsePartsRejects(t *testing.T) {
	cases := []struct {
		name                            string
		daily, close, intimate, private string
		want                            string
	}{
		{"四档全空", "", "", "", "", "至少要有一个部位"},
		{"跨档重名", "手", "手", "", "", "重复出现"},
		{"同档重名", "手\n手", "", "", "", "重复出现"},
		{"部位名过长", strings.Repeat("长", maxPartRunes+1), "", "", "", "过长"},
		{"条数超上限", strings.Join(genNames(maxParts+1), "\n"), "", "", "", "超出上限"},
		{"总字节超上限", strings.Repeat("手\n", maxPartsBytes), "", "", "", "部位表过长"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseParts("", c.daily, c.close, c.intimate, c.private)
			if err == nil {
				t.Fatalf("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("报错 = %v，期望含 %q", err, c.want)
			}
		})
	}
}

// 校验失败必须是 error 而不是「跳过并记日志」：SetConfig 会把 Init 的错误回给设置页，
// 只记日志的话用户会以为那一行生效了。
func TestParsePartsErrorNamesTheOffendingGroup(t *testing.T) {
	_, err := parseParts("", "", "", "腰", "腰")
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "亲密部位") || !strings.Contains(err.Error(), "私密部位") {
		t.Errorf("报错 = %v，期望指出是哪两档冲突", err)
	}
}

func TestFindPart(t *testing.T) {
	parts := []Part{{Name: "手腕", Privacy: privacyDaily}, {Name: "腰", Privacy: privacyIntimate}}
	if p, ok := findPart(parts, "  手 腕 "); !ok || p.Name != "手腕" {
		t.Errorf("空白应被忽略，得到 %+v ok=%v", p, ok)
	}
	if _, ok := findPart(parts, "膝盖"); ok {
		t.Error("表外的部位不该匹配上")
	}
}

func genNames(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "部位"+string(rune('A'+i%26))+string(rune('a'+i/26)))
	}
	return out
}
