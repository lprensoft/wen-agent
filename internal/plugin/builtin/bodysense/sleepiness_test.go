package bodysense

import (
	"strings"
	"testing"
	"time"

	"wen/internal/i18n"
	"wen/internal/plugin"
	"wen/internal/prompt"
)

func TestComputeSleepiness(t *testing.T) {
	cases := []struct {
		name          string
		load          float64
		onset, hour   int
		wantPressure  int
		wantClock     int
		wantScoreAtMo int // 期望总分的下限
	}{
		{"白天、负载在起算点以下", 0.4, 50, 12, 0, 0, 0},
		{"白天、负载过半", 0.75, 50, 12, 35, 0, 35},
		{"到了压缩点", 1.0, 50, 12, pressureWeight, 0, pressureWeight},
		{"超过压缩点按满分", 1.4, 50, 12, pressureWeight, 0, pressureWeight},
		{"起算 100 只按钟点", 1.0, 100, 12, 0, 0, 0},
		{"深夜、负载很轻", 0.1, 50, 3, 0, 45, 45},
		{"深夜叠上压缩点封顶", 1.0, 50, 3, pressureWeight, 45, stateMax},
	}
	for _, c := range cases {
		got := computeSleepiness(c.load, c.onset, c.hour)
		if got.Pressure != c.wantPressure || got.Clock != c.wantClock || got.Score < c.wantScoreAtMo || got.Score > stateMax {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}
}

// fakeStatus 返回一个用量为 load 倍压缩点的会话快照。
func fakeStatus(load float64) plugin.StatusFunc {
	return func(string) (plugin.StatusInfo, error) {
		return plugin.StatusInfo{HasSession: true, CompactAt: 1000, EstTokens: int(load * 1000), MeasuredTokens: -1}, nil
	}
}

func sleepyPlugin(t *testing.T, cfg map[string]any, load float64, hour int) *Plugin {
	t.Helper()
	p := New()
	p.now = func() time.Time { return time.Date(2026, 10, 22, hour, 0, 0, 0, time.Local) }
	p.roll = func() float64 { return 1 }
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir(), Status: fakeStatus(load)}, cfg); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTurnPromptShowsSleepinessFromContextLoad(t *testing.T) {
	p := sleepyPlugin(t, nil, 1.0, 12)
	got, err := p.TurnPrompt(scoped("", nil), plugin.TurnEvent{SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[当前身体状态]") || !strings.Contains(got, "困意：很困，眼皮发沉（醒了很久）") {
		t.Errorf("到了压缩点应当很困：%q", got)
	}

	// 负载很轻的白天不注入
	p = sleepyPlugin(t, nil, 0.2, 12)
	if got, _ := p.TurnPrompt(scoped("", nil), plugin.TurnEvent{SessionID: "s"}); got != "" {
		t.Errorf("清醒时不该注入：%q", got)
	}

	// 取不到会话（没有会话 id）时只按钟点算：深夜照样困
	p = sleepyPlugin(t, nil, 1.0, 2)
	got, _ = p.TurnPrompt(scoped("", nil), plugin.TurnEvent{})
	if !strings.Contains(got, "困了（夜深了）") {
		t.Errorf("深夜只按钟点：%q", got)
	}
}

func TestSleepinessCanBeTurnedOff(t *testing.T) {
	p := sleepyPlugin(t, map[string]any{"sleepiness": false}, 1.0, 3)
	if got, _ := p.TurnPrompt(scoped("", nil), plugin.TurnEvent{SessionID: "s"}); got != "" {
		t.Errorf("关掉困意后不该注入：%q", got)
	}
	if strings.Contains(p.SystemPrompt(), "[困意]") {
		t.Error("关掉困意后判据也不该注入")
	}
	if !strings.Contains(sleepyPlugin(t, nil, 0, 12).SystemPrompt(), "[困意]") {
		t.Error("打开困意时应注入判据")
	}
}

func TestSleepinessWaveHasCooldown(t *testing.T) {
	p := sleepyPlugin(t, nil, 1.0, 12)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	p.now = func() time.Time { return at }
	p.roll = func() float64 { return 0 } // 每次都想冒
	s := p.snapshot()

	if line := p.sleepinessLine(s, "s"); !strings.Contains(line, waveLineZH) {
		t.Fatalf("第一阵应当冒出来：%q", line)
	}
	at = at.Add(5 * time.Minute)
	if line := p.sleepinessLine(s, "s"); strings.Contains(line, waveLineZH) {
		t.Errorf("冷却期内不该再冒：%q", line)
	}
	at = at.Add(waveCooldown)
	if line := p.sleepinessLine(s, "s"); !strings.Contains(line, waveLineZH) {
		t.Errorf("冷却过后应当能再冒：%q", line)
	}
}

// [困意] 的判据引用了「一阵困意」那一句，真正注入的是 renderSleepiness 的输出。
// 两处分头改就会让判据指向一句上下文里根本不会出现的话。逐语言盯着。
func TestSleepGuideQuotesWave(t *testing.T) {
	for _, l := range i18n.Languages() {
		wave := strings.TrimRight(strings.TrimSpace(prompt.T(l.Code, "prompt.body_sense.sleepiness.wave", waveLineZH)), "。.")
		line := renderSleepiness(l.Code, computeSleepiness(1, 50, 12), true)
		if !strings.Contains(line, wave) {
			t.Fatalf("%s: 渲染结果里没有那一句：%q", l.Code, line)
		}
		if wave == "" || !strings.Contains(sleepGuideFor(l.Code), wave) {
			t.Errorf("%s: [困意] 没有引用真正的那一句 %q:\n%s", l.Code, wave, sleepGuideFor(l.Code))
		}
	}
}
