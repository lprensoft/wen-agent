package weather

import (
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func stateTestPlugin(t *testing.T, now time.Time) *Plugin {
	t.Helper()
	p := withObs(t, "杭州", now.Add(-5*time.Minute))
	p.lang = "zh"
	p.personaLoc = "杭州"
	p.sameCity = true
	p.stale = time.Hour

	p.dataMu.Lock()
	rep := p.obs["杭州"].cur
	rep.Place = "杭州 · 浙江省 · 中国"
	rep.TempC = 12
	rep.Today = DayInfo{Date: now.Format("2006-01-02"), Condition: condCloudy, MinC: 8, MaxC: 18}
	p.obs["杭州"].cur = rep
	p.dataMu.Unlock()
	return p
}

func TestStateShowsCurrentWeather(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	got := secs[0]
	if got.Title != "天气" || !got.Global || got.UpdatedAt.IsZero() {
		t.Errorf("section = %+v", got)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %+v", got.Rows)
	}
	row := got.Rows[0]
	if row.Label != "同城" || !strings.Contains(row.Value, "多云") || !strings.Contains(row.Value, "12℃") {
		t.Errorf("row = %+v", row)
	}
	if !strings.Contains(row.Detail, "杭州") || !strings.Contains(row.Detail, "今天 多云 8~18℃") {
		t.Errorf("detail = %q", row.Detail)
	}
}

func TestStateMarksStaleReading(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 0, 0, 0, time.Local)
	p := withObs(t, "杭州", now.Add(-2*time.Hour))
	p.lang = "zh"
	p.personaLoc = "杭州"
	p.sameCity = true
	p.stale = time.Hour

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	row := secs[0].Rows[0]
	if row.Tone != plugin.StateToneWarn || !strings.Contains(row.Detail, "已过期") {
		t.Errorf("row = %+v", row)
	}
}

func TestStateWithoutCity(t *testing.T) {
	p := New()
	p.lang = "zh"
	p.sameCity = true

	secs := p.State(plugin.NewStateQuery("zh", false, time.Now()))
	if len(secs) != 1 || secs[0].Empty == "" || len(secs[0].Rows) != 0 {
		t.Fatalf("State = %+v，期望一块带 Empty 的空块", secs)
	}
	if !secs[0].Global {
		t.Error("天气块应标为 Global")
	}
}

func TestStatePinnedOnlySkipsEverything(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 0, 0, 0, time.Local)
	p := stateTestPlugin(t, now)
	if secs := p.State(plugin.NewStateQuery("zh", true, now)); len(secs) != 0 {
		t.Fatalf("PinnedOnly = %+v，天气不该进置顶条", secs)
	}
}

func TestStateSeparatesLocations(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 0, 0, 0, time.Local)
	p := withObs(t, "杭州", now.Add(-5*time.Minute))
	p.lang = "zh"
	p.personaLoc = "杭州"
	p.userLoc = "北京"
	p.sameCity = false
	p.stale = time.Hour

	p.dataMu.Lock()
	o := p.entryLocked("北京")
	o.cur = Report{Place: "北京 · 中国", Condition: condClear, TempC: 8, Fetched: now.Add(-3 * time.Minute)}
	o.curOK = true
	p.dataMu.Unlock()

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 2 {
		t.Fatalf("State = %+v", secs)
	}
	if secs[0].Rows[0].Label != "角色" || !strings.Contains(secs[0].Rows[0].Value, "多云") {
		t.Errorf("角色行 = %+v", secs[0].Rows[0])
	}
	if secs[0].Rows[1].Label != "我" || !strings.Contains(secs[0].Rows[1].Value, "晴") {
		t.Errorf("我行 = %+v", secs[0].Rows[1])
	}
	if !secs[0].UpdatedAt.Equal(now.Add(-3 * time.Minute)) {
		t.Errorf("UpdatedAt = %v，期望取最新的一次观测", secs[0].UpdatedAt)
	}
}

func TestStatePendingWithError(t *testing.T) {
	now := time.Date(2026, 3, 5, 14, 0, 0, 0, time.Local)
	p := New()
	p.lang = "zh"
	p.personaLoc = "杭州"
	p.sameCity = true
	p.stale = time.Hour
	p.dataMu.Lock()
	p.entryLocked("杭州").lastErr = "解析地址失败"
	p.dataMu.Unlock()

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	row := secs[0].Rows[0]
	if row.Value != "尚未取得" || row.Detail != "杭州 · 解析地址失败" || row.Tone != plugin.StateToneWarn {
		t.Errorf("row = %+v", row)
	}
}

// daily 的日期按地点当地时区给：服务器已经是 3 月 5 日、纽约还是 3 月 4 日时，
// 不能把刚取回的纽约「今天」预报过滤掉。
func TestStateUsesLocationDateForToday(t *testing.T) {
	now := time.Date(2026, 3, 5, 2, 0, 0, 0, time.FixedZone("server", 8*3600))
	p := withObs(t, "纽约", now.Add(-5*time.Minute))
	p.lang = "zh"
	p.personaLoc = "纽约"
	p.sameCity = true
	p.stale = time.Hour

	p.dataMu.Lock()
	rep := p.obs["纽约"].cur
	rep.Place = "纽约 · 美国"
	rep.UTCOffsetSeconds = -5 * 3600
	rep.UTCOffsetSet = true
	rep.Today = DayInfo{Date: "2026-03-04", Condition: condCloudy, MinC: 1, MaxC: 8}
	p.obs["纽约"].cur = rep
	p.dataMu.Unlock()

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 1 {
		t.Fatalf("State = %+v", secs)
	}
	if detail := secs[0].Rows[0].Detail; !strings.Contains(detail, "今天 多云 1~8℃") {
		t.Errorf("detail = %q，期望按纽约当地日期带上今日概要", detail)
	}
}
