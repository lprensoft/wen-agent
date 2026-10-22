package plugin

import "testing"

// CompactLoad 与核心自动压缩同一判据：实测与估算取较大者，除以触发点。
func TestCompactLoad(t *testing.T) {
	cases := []struct {
		name string
		info StatusInfo
		want float64
	}{
		{"没有会话", StatusInfo{CompactAt: 1000, EstTokens: 500}, 0},
		{"触发点未知", StatusInfo{HasSession: true, EstTokens: 500}, 0},
		{"无实测时用估算", StatusInfo{HasSession: true, CompactAt: 1000, MeasuredTokens: -1, EstTokens: 500}, 0.5},
		{"取较大者", StatusInfo{HasSession: true, CompactAt: 1000, MeasuredTokens: 800, EstTokens: 500}, 0.8},
	}
	for _, c := range cases {
		if got := c.info.CompactLoad(); got != c.want {
			t.Errorf("%s: CompactLoad = %v, want %v", c.name, got, c.want)
		}
	}
}
