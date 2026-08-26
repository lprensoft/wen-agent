package agent

import (
	"strings"
	"testing"
	"time"

	"wen/internal/i18n"
	"wen/internal/prompt"
)

func TestEnvContextIsStableAcrossTurns(t *testing.T) {
	for _, l := range i18n.Languages() {
		got := envContext(l.Code, `D:\work`)
		if !strings.Contains(got, `D:\work`) {
			t.Errorf("%s: 环境块缺少工作目录:\n%s", l.Code, got)
		}
		// 时间不能放在这里：它每轮都变，会连带作废整段 system 与历史的缓存前缀，
		// 而且离生成位置最远，最容易被历史里出现过的旧时刻盖过去。它归 turnStateBlock。
		unwanted := []string{prompt.T(l.Code, "prompt.core.turn_state.now", turnStateNowZH)}
		unwanted = append(unwanted, weekdayName(l.Code, time.Now()))
		for _, u := range unwanted {
			if strings.Contains(got, u) {
				t.Errorf("%s: 环境块必须整轮不变，不该出现 %q:\n%s", l.Code, u, got)
			}
		}
		if got != envContext(l.Code, `D:\work`) {
			t.Errorf("%s: 同样入参两次调用应当逐字节相同，否则缓存前缀每轮作废", l.Code)
		}
	}
}
