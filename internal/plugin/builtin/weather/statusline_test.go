package weather

import (
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

// 三种「角色不提天气」的情况在对话里看起来一样，状态行是唯一能把它们分开的地方。
func TestStatusLineNotConfigured(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"persona_location": ""}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := p.StatusLines(""); len(got) != 1 || !strings.Contains(got[0], "未设置城市") {
		t.Errorf("未配置时 = %v", got)
	}
}

func TestStatusLineSameCity(t *testing.T) {
	stubServer(t, geoOK, fcOK)
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "same_city": true, "refresh_minutes": 30, "stale_minutes": 60,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop() // 停掉后台循环，免得它把桩数据写进来

	if got := p.StatusLines(""); len(got) != 1 || !strings.Contains(got[0], "尚未取得") {
		t.Errorf("未取到数据时 = %v", got)
	}

	setObs(p, "杭州", report("杭州 · 浙江 · 中国", "小雨", 18, 0))
	got := p.StatusLines("")
	if len(got) != 1 || !strings.Contains(got[0], "杭州 小雨") || !strings.Contains(got[0], "每 30 分钟刷新") {
		t.Errorf("正常时 = %v", got)
	}
	if strings.Contains(got[0], "浙江") {
		t.Error("状态行是给人扫一眼的，不该带上完整行政区划")
	}
}

// 两处地点时，一处失效不代表另一处也没在用——过期要标在具体那一处上。
func TestStatusLineTwoCitiesMarksStalePerSide(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "same_city": false, "user_location": "上海",
		"refresh_minutes": 30, "stale_minutes": 60,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop()
	setObs(p, "杭州", report("杭州", "小雨", 18, 0))
	setObs(p, "上海", report("上海", "阴", 27, 3*time.Hour))

	got := p.StatusLines("")
	if len(got) != 1 {
		t.Fatalf("应当只有一行，实际 %v", got)
	}
	line := got[0]
	if !strings.Contains(line, "角色 杭州") || !strings.Contains(line, "我 上海") {
		t.Errorf("两处都应当报出来：%s", line)
	}
	if !strings.Contains(line, "上海 阴 27℃（已过期，不注入）") {
		t.Errorf("过期应当标在上海这一处：%s", line)
	}
	if strings.Contains(line, "杭州 小雨 18℃（已过期") {
		t.Errorf("杭州仍在注入，不该标过期：%s", line)
	}
}

// 一直取不到时，状态行要带上失败原因——否则「尚未取得」解释不了任何事。
func TestStatusLineCarriesLastError(t *testing.T) {
	stubServer(t, "", "")
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"persona_location": "杭州"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop() // 先停掉后台循环：它启动时就会取一次，会把自己的失败原因写进来
	p.noteError("杭州", errStub{})

	got := p.StatusLines("")
	if len(got) != 1 || !strings.Contains(got[0], "桩错误") {
		t.Errorf("状态行 = %v", got)
	}
}

type errStub struct{}

func (errStub) Error() string { return "桩错误" }

func TestShortPlace(t *testing.T) {
	if got := shortPlace("杭州 · 浙江省 · 中国"); got != "杭州" {
		t.Errorf("shortPlace = %q", got)
	}
	if got := shortPlace("杭州"); got != "杭州" {
		t.Errorf("无分隔时应当原样返回，实际 %q", got)
	}
}

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "刚刚更新"},
		{5 * time.Minute, "5 分钟前更新"},
		{3 * time.Hour, "3 小时前更新"},
		{50 * time.Hour, "2 天前更新"},
	}
	for _, c := range cases {
		if got := humanizeAge(c.d); got != c.want {
			t.Errorf("humanizeAge(%v) = %q，期望 %q", c.d, got, c.want)
		}
	}
}
