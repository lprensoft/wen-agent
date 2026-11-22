package weather

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func withObs(t *testing.T, loc string, fetched time.Time) *Plugin {
	t.Helper()
	p := New()
	p.dataMu.Lock()
	o := p.entryLocked(loc)
	o.place, o.placeOK = Place{Name: "杭州 · 浙江省 · 中国", Lat: 30.29, Lon: 120.16}, true
	o.cur, o.curOK = Report{Place: "杭州", Condition: "多云", TempC: 21.5, Fetched: fetched}, true
	p.dataMu.Unlock()
	return p
}

// 观测缓存要能跨重启活下来：重启后既不该有空窗，也不该白烧一次 API 调用。
func TestObservationSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	fetched := time.Now().Add(-5 * time.Minute).Truncate(time.Second)

	withObs(t, "杭州", fetched).save(dir)

	p2 := New()
	p2.loadInto(dir, []string{"杭州"})

	rep, ok := p2.fresh("杭州", 60*time.Minute, time.Now())
	if !ok {
		t.Fatal("重启后应能取到上次的观测")
	}
	if rep.Condition != "多云" || rep.TempC != 21.5 {
		t.Errorf("观测内容不对: %+v", rep)
	}
	if !rep.Fetched.Equal(fetched) {
		t.Errorf("观测时刻 = %v, want %v（过期判定全靠它）", rep.Fetched, fetched)
	}
	// 地名解析结果一并恢复，重启后不必再问一次地理编码接口
	p2.dataMu.RLock()
	o := p2.obs["杭州"]
	p2.dataMu.RUnlock()
	if o == nil || !o.placeOK || o.place.Name == "" {
		t.Error("地名解析结果未恢复")
	}
}

// 加载回来的旧观测照样受过期时限约束——不能把来历久远的天气当成此刻的。
func TestLoadedObservationStillExpires(t *testing.T) {
	dir := t.TempDir()
	withObs(t, "杭州", time.Now().Add(-3*time.Hour)).save(dir)

	p2 := New()
	p2.loadInto(dir, []string{"杭州"})
	if _, ok := p2.fresh("杭州", 30*time.Minute, time.Now()); ok {
		t.Error("超过过期时限的观测不该被取用")
	}
}

// 缓存还新鲜时，重启后要等到该刷新的时刻，而不是立刻抢一次。
func TestUntilRefreshUsesLastObservation(t *testing.T) {
	now := time.Now()
	p := withObs(t, "杭州", now.Add(-10*time.Minute))

	got := p.untilRefresh([]string{"杭州"}, 30*time.Minute, now)
	if got < 19*time.Minute || got > 21*time.Minute {
		t.Errorf("应还等约 20 分钟，得到 %v", got)
	}

	// 已经过了刷新间隔就立刻取
	p2 := withObs(t, "杭州", now.Add(-2*time.Hour))
	if got := p2.untilRefresh([]string{"杭州"}, 30*time.Minute, now); got != 0 {
		t.Errorf("过期后应立刻刷新，得到 %v", got)
	}

	// 有一处没有数据就立刻取：那一处正处在空窗里
	p3 := withObs(t, "杭州", now)
	if got := p3.untilRefresh([]string{"杭州", "上海"}, 30*time.Minute, now); got != 0 {
		t.Errorf("有城市尚无观测时应立刻刷新，得到 %v", got)
	}
}

// 没有观测时刻的条目直接丢弃：没有时刻就无从判断新鲜与否。
func TestLoadRejectsUndatedReport(t *testing.T) {
	dir := t.TempDir()
	withObs(t, "杭州", time.Time{}).save(dir)

	p2 := New()
	p2.loadInto(dir, []string{"杭州"})
	if _, ok := p2.fresh("杭州", 30*time.Minute, time.Now()); ok {
		t.Error("没有观测时刻的条目不该被采用")
	}
}

// 拿不到持久化目录时拒绝启用，与其余需要落盘的插件一致（见 CLAUDE.md 的持久化约定）。
// 真实程序里 StateDir 永远由 Manager 给出，这条守的是约定本身：
// 破一个例，下一个插件就不知道该照谁写。
func TestInitRequiresStateDir(t *testing.T) {
	p := New()
	err := p.Init(plugin.InitContext{}, map[string]any{"persona_location": "杭州"})
	if err == nil {
		t.Fatal("没有持久化目录时应拒绝启用")
	}
	if !strings.Contains(err.Error(), "持久化目录") {
		t.Errorf("错误信息应说明缺的是什么: %v", err)
	}
}

// 损坏的缓存文件被忽略，插件照常工作。
func TestCorruptStateIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(filepath.Join(dir, stateFile), "{ 这不是 JSON"); err != nil {
		t.Fatal(err)
	}
	p := New()
	p.loadInto(dir, []string{"杭州"})
	if _, ok := p.fresh("杭州", 30*time.Minute, time.Now()); ok {
		t.Error("损坏的缓存不该产出观测")
	}
}

// 时区偏移要跟着观测一起落盘：重启后判断「今天」不能退回服务器时区。
func TestObservationKeepsOffsetAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	p := withObs(t, "纽约", time.Now().Add(-time.Minute))
	p.dataMu.Lock()
	rep := p.obs["纽约"].cur
	rep.UTCOffsetSeconds = -5 * 3600
	rep.UTCOffsetSet = true
	rep.Timezone = "America/New_York"
	p.obs["纽约"].cur = rep
	p.dataMu.Unlock()
	p.save(dir)

	p2 := New()
	p2.loadInto(dir, []string{"纽约"})
	rep, ok := p2.fresh("纽约", time.Hour, time.Now())
	if !ok || !rep.UTCOffsetSet || rep.UTCOffsetSeconds != -5*3600 || rep.Timezone != "America/New_York" {
		t.Fatalf("重启后时区 = (%v, %d, %q)，ok=%v", rep.UTCOffsetSet, rep.UTCOffsetSeconds, rep.Timezone, ok)
	}
}
