package health

import (
	"math"
	"strings"
	"testing"
	"time"
)

// t0 是测试里固定的发作时刻：早上八点，之后按日历日算「第几天」才稳定。
var t0 = time.Date(2026, 8, 20, 8, 0, 0, 0, time.Local)

func cond(name string, peak, days int, care string, onset time.Time) Condition {
	return Condition{ID: "1", Name: name, Peak: peak, Days: days, Care: care, Onset: onset, ProgressAt: onset}
}

func hours(h float64) time.Duration { return time.Duration(h * float64(time.Hour)) }

func TestCurveRisePeakFall(t *testing.T) {
	c := cond("着凉", 55, 4, careTough, t0)

	if v := c.view(t0.Add(-time.Hour)); v.Phase != phasePending || v.Severity != 0 {
		t.Errorf("发作前应是苗头: %+v", v)
	}
	// 刚发作：不从零起跳，但还在加重
	v := c.view(t0.Add(time.Hour))
	if v.Phase != phaseRising || v.Severity < 13 || v.Severity > 20 || v.Day != 1 {
		t.Errorf("发作一小时后 = %+v", v)
	}
	// 病程前三成处到达峰值
	v = c.view(t0.Add(hours(0.3 * 4 * 24)))
	if v.Severity != 55 {
		t.Errorf("峰值处严重度 = %d, want 55", v.Severity)
	}
	// 之后线性回落
	v = c.view(t0.Add(48 * time.Hour))
	if v.Phase != phaseFalling || v.Severity != 39 {
		t.Errorf("第三天 = %+v, want falling/39", v)
	}
	// 走完病程
	v = c.view(t0.Add(4 * 24 * time.Hour))
	if v.Phase != phaseRecovered || v.Severity != 0 {
		t.Errorf("四天后 = %+v, want recovered", v)
	}
	if !c.recoveryAt().Equal(t0.Add(4 * 24 * time.Hour)) {
		t.Errorf("硬扛的痊愈时刻 = %v", c.recoveryAt())
	}
}

func TestCareFactorsOnlySpeedUpRecovery(t *testing.T) {
	var at [3]time.Time
	for i, care := range []string{careTough, careMeds, careDoctor} {
		c := cond("着凉", 55, 4, care, t0)
		at[i] = c.recoveryAt()
		// 加重段不受处理方式影响：峰值时刻三种都一样
		if v := c.view(t0.Add(hours(0.3 * 96))); v.Severity != 55 {
			t.Errorf("%s 的峰值处严重度 = %d", care, v.Severity)
		}
	}
	if !at[1].Before(at[0]) || !at[2].Before(at[1]) {
		t.Errorf("恢复快慢应为 就医 < 吃药 < 硬扛: %v", at)
	}
	// 硬扛 4 天；吃药 1.2 + 2.8/1.6 = 2.95 天；就医 1.2 + 2.8/2.5 = 2.32 天
	for i, want := range []float64{96, 70.8, 55.68} {
		if got := at[i].Sub(t0).Hours(); math.Abs(got-want) > 0.01 {
			t.Errorf("第 %d 种处理方式的病程 = %.2f 小时, want %.2f", i, got, want)
		}
	}
}

func TestApplyCareMidCourseKeepsSeverity(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, err := s.Add("着凉", sevModerate.ZH(), t0, 4, careTough, t0, Limits{}); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(48 * time.Hour)
	before, _ := s.Current(now)
	c, changes, err := s.Apply("", Update{Care: strp(careMeds)}, now, Limits{})
	if err != nil || len(changes) != 1 || changes[0] != "处理方式 硬扛→吃药" {
		t.Fatalf("改处理方式: %v, %v", changes, err)
	}
	after, _ := s.Current(now)
	if before.Active[0].Severity != after.Active[0].Severity {
		t.Errorf("换处理方式那一刻严重度不该跳变: %d → %d", before.Active[0].Severity, after.Active[0].Severity)
	}
	if !c.recoveryAt().Before(before.Active[0].Cond.recoveryAt()) {
		t.Error("吃药后应更早痊愈")
	}
	// 还没发作时改处理方式：基准仍是发作时刻，不折算
	s2 := NewStore("", t.TempDir())
	s2.Add("着凉", sevModerate.ZH(), t0.Add(5*time.Hour), 4, careTough, t0, Limits{})
	c2, _, err := s2.Apply("", Update{Care: strp(careDoctor)}, t0.Add(time.Hour), Limits{})
	if err != nil || !c2.ProgressAt.Equal(t0.Add(5*time.Hour)) || c2.Progress != 0 {
		t.Errorf("发作前改处理方式不该动基准: %+v, %v", c2, err)
	}
}

func TestApplySeverityWorseAndBetter(t *testing.T) {
	s := NewStore("", t.TempDir())
	s.Add("着凉", sevMild.ZH(), t0, 4, careTough, t0, Limits{MaxPeak: 100})
	now := t0.Add(48 * time.Hour)

	// 加重：以新峰值从此刻重新起算好转段
	c, changes, err := s.Apply("", Update{Severity: strp(sevSevere.ZH())}, now, Limits{MaxPeak: 100})
	if err != nil || c.Peak != 85 || c.Progress != riseEnd || !c.ProgressAt.Equal(now) {
		t.Fatalf("加重: %+v, %v, %v", c, changes, err)
	}
	if v := c.view(now); band("", v.Severity) != sevSevere.ZH() {
		t.Errorf("加重后此刻应为病倒了: %+v", v)
	}
	// 好转：峰值不变，进度拨到严重度恰好等于新档
	later := now.Add(time.Hour)
	c, _, err = s.Apply("", Update{Severity: strp(sevMild.ZH())}, later, Limits{MaxPeak: 100})
	if err != nil || c.Peak != 85 {
		t.Fatalf("好转: %+v, %v", c, err)
	}
	if v := c.view(later); v.Severity != 30 || v.Phase != phaseFalling {
		t.Errorf("好转后此刻严重度应恰为 30: %+v", v)
	}
	// 封顶：改严重度同样受上限约束，并把规则写进回执
	_, changes, err = s.Apply("", Update{Severity: strp(sevSevere.ZH())}, later, Limits{MaxPeak: 35})
	if err != nil || len(changes) != 2 || !strings.Contains(changes[1], "按上限收成「有点不舒服」") {
		t.Errorf("封顶回执 = %v, %v", changes, err)
	}
}

func TestAddRejectsWithRules(t *testing.T) {
	s := NewStore("", t.TempDir())
	lim := Limits{Cooldown: 7 * 24 * time.Hour, MaxConditions: 1, MaxPeak: 65}

	c, err := s.Add("着凉", sevSevere.ZH(), t0, 3, careTough, t0, lim)
	if err != nil || c.Peak != 65 || !c.Cued {
		t.Fatalf("登记: %+v, %v", c, err)
	}
	// 同时数量上限
	_, err = s.Add("头疼", sevMild.ZH(), t0, 1, careTough, t0, lim)
	if err == nil || !strings.Contains(err.Error(), "最多记 1 个") || !strings.Contains(err.Error(), "update_condition") {
		t.Errorf("超出数量上限应拒绝并说明: %v", err)
	}
	// 同名
	if _, err := s.Add("着凉", sevMild.ZH(), t0, 1, careTough, t0, Limits{}); err == nil || !strings.Contains(err.Error(), "同名") {
		t.Errorf("同名应拒绝: %v", err)
	}
	// 标记痊愈后进入冷却
	if _, _, err := s.Apply("着凉", Update{Recovered: true}, t0.Add(24*time.Hour), lim); err != nil {
		t.Fatal(err)
	}
	_, err = s.Add("头疼", sevMild.ZH(), t0.Add(48*time.Hour), 1, careTough, t0.Add(48*time.Hour), lim)
	if err == nil || !strings.Contains(err.Error(), "7 天内不再添新状况") || !strings.Contains(err.Error(), "着凉") {
		t.Errorf("冷却期内应拒绝并说明规则: %v", err)
	}
	// 冷却期过
	if _, err := s.Add("头疼", sevMild.ZH(), t0.Add(9*24*time.Hour), 1, careTough, t0.Add(9*24*time.Hour), lim); err != nil {
		t.Errorf("冷却期过后应可登记: %v", err)
	}
	// 参数校验
	for name, args := range map[string][]any{
		"空名":   {"  ", sevMild.ZH(), 1, careTough},
		"名字过长": {strings.Repeat("病", maxNameRunes+1), sevMild.ZH(), 1, careTough},
		"严重度":  {"x", "很重", 1, careTough},
		"处理方式": {"x", sevMild.ZH(), 1, "躺平"},
		"天数过长": {"x", sevMild.ZH(), maxDays + 1, careTough},
	} {
		if _, err := NewStore("", t.TempDir()).Add(args[0].(string), args[1].(string), t0, args[2].(int), args[3].(string), t0, Limits{}); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestNaturalRecoveryFeedsCooldown(t *testing.T) {
	s := NewStore("", t.TempDir())
	s.Add("着凉", sevMild.ZH(), t0, 2, careTough, t0, Limits{})
	// 读取不回写：文件里还留着，但折算后已不算活跃
	snap, _ := s.Current(t0.Add(3 * 24 * time.Hour))
	if len(snap.Active) != 0 || !snap.LastRecovered.Equal(t0.Add(48*time.Hour)) || snap.LastName != "着凉" {
		t.Errorf("走完病程后的全貌 = %+v", snap)
	}
	lim := Limits{Cooldown: 7 * 24 * time.Hour}
	if _, err := s.Add("头疼", sevMild.ZH(), t0.Add(3*24*time.Hour), 1, careTough, t0.Add(3*24*time.Hour), lim); err == nil {
		t.Error("自然痊愈同样进入冷却")
	}
}

func TestPickByName(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, _, err := s.Apply("", Update{Recovered: true}, t0, Limits{}); err == nil || !strings.Contains(err.Error(), "没有记下") {
		t.Errorf("空库: %v", err)
	}
	s.Add("着凉", sevMild.ZH(), t0, 3, careTough, t0, Limits{})
	s.Add("头疼", sevMild.ZH(), t0, 1, careTough, t0, Limits{})
	if _, _, err := s.Apply("", Update{Recovered: true}, t0, Limits{}); err == nil || !strings.Contains(err.Error(), "着凉、头疼") {
		t.Errorf("多条时不指名应列出候选: %v", err)
	}
	if _, _, err := s.Apply("发烧", Update{Recovered: true}, t0, Limits{}); err == nil || !strings.Contains(err.Error(), "没有叫「发烧」") {
		t.Errorf("不存在的名字: %v", err)
	}
	if _, _, err := s.Apply("着凉", Update{}, t0, Limits{}); err == nil || !strings.Contains(err.Error(), "至少给一项") {
		t.Errorf("空改动: %v", err)
	}
}

func TestRenderFourForms(t *testing.T) {
	pending := cond("着凉", 55, 4, careTough, t0.Add(5*time.Hour))
	if got := renderView("", pending.view(t0), t0); got != "着凉的苗头：预计约 5 小时后发作，到时大概会难受。" {
		t.Errorf("苗头 = %q", got)
	}
	rising := cond("着凉", 55, 4, careTough, t0)
	now := t0.Add(time.Hour)
	if got := renderView("", rising.view(now), now); got != "着凉第 1 天，有点不舒服，还在往重里走（硬扛着），预计再四五天好。" {
		t.Errorf("在加重 = %q", got)
	}
	peak := cond("感冒", 55, 4, careMeds, t0)
	now = t0.Add(hours(28.8))
	if got := renderView("", peak.view(now), now); got != "感冒第 2 天，难受（吃了药），预计再两三天好。" {
		t.Errorf("峰值档 = %q", got)
	}
	falling := cond("感冒", 55, 4, careTough, t0)
	now = t0.Add(60 * time.Hour)
	if got := renderView("", falling.view(now), now); got != "感冒第 3 天，从难受转成有点不舒服（硬扛着），预计再两三天好。" {
		t.Errorf("转轻 = %q", got)
	}
}

func TestRenderSnapshotWindowsAndGuide(t *testing.T) {
	cooldown := 7 * 24 * time.Hour
	// 冷却期内：还虚着
	snap := Snapshot{LastRecovered: t0.Add(-2 * 24 * time.Hour), LastName: "感冒"}
	if got := renderSnapshot("", snap, t0, cooldown); got != "刚病好没几天（感冒，2 天前痊愈），还有点虚，精神没全回来；这阵子不会再添新状况。" {
		t.Errorf("冷却期 = %q", got)
	}
	// 冷却期后同样长的一段：易感
	snap.LastRecovered = t0.Add(-10 * 24 * time.Hour)
	if got := renderSnapshot("", snap, t0, cooldown); got != "前阵子刚病过一场（感冒），抵抗力还弱，最近容易着凉。" {
		t.Errorf("易感期 = %q", got)
	}
	// 再往后：什么都不注入
	snap.LastRecovered = t0.Add(-15 * 24 * time.Hour)
	if got := renderSnapshot("", snap, t0, cooldown); got != "" {
		t.Errorf("窗口之外不该注入: %q", got)
	}
	// 只有苗头：不附表现指引；发作中才附
	pend := cond("着凉", 55, 4, careTough, t0.Add(5*time.Hour))
	if got := renderSnapshot("", Snapshot{Active: []View{pend.view(t0)}}, t0, cooldown); strings.Contains(got, guideLineFor("")) {
		t.Errorf("苗头不该附表现指引: %q", got)
	}
	sick := cond("着凉", 55, 4, careTough, t0)
	now := t0.Add(time.Hour)
	got := renderSnapshot("", Snapshot{Active: []View{sick.view(now)}, LastRecovered: t0.Add(-24 * time.Hour)}, now, cooldown)
	if !strings.HasSuffix(got, "\n"+guideLineFor("")) || strings.Contains(got, "刚病好") {
		t.Errorf("发作中应附指引、且不再提上一场: %q", got)
	}
}

func TestRecoverText(t *testing.T) {
	for want, d := range map[string]time.Duration{
		"预计今天内就好":     6 * time.Hour,
		"预计明天就好":      30 * time.Hour,
		"预计再两三天好":     40 * time.Hour,
		"预计再三四天好":     60 * time.Hour,
		"预计再五六天好":     5 * 24 * time.Hour,
		"预计再一周左右好":    8 * 24 * time.Hour,
		"预计再 12 天左右好": 12 * 24 * time.Hour,
	} {
		if got := recoverText("", d); got != want {
			t.Errorf("recoverText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestBandsAndCaps(t *testing.T) {
	for v, want := range map[int]string{0: "", 1: sevMild.ZH(), 35: sevMild.ZH(), 36: sevModerate.ZH(), 65: sevModerate.ZH(), 66: sevSevere.ZH(), 100: sevSevere.ZH()} {
		if got := band("", v); got != want {
			t.Errorf("band(%d) = %q", v, got)
		}
	}
	// 三档的代表值落在各自的档内，封顶值是各档的上沿
	for _, w := range []string{sevMild.ZH(), sevModerate.ZH(), sevSevere.ZH()} {
		p, _ := peakFor(w)
		if band("", p) != w {
			t.Errorf("%s 的代表值 %d 不在本档", w, p)
		}
	}
	if band("", capFor(capMild)) != sevMild.ZH() || band("", capFor(capModerate)) != sevModerate.ZH() || band("", capFor(capSevere)) != sevSevere.ZH() {
		t.Error("封顶值应落在对应档的上沿")
	}
}

func TestLoadTolerates(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, err := s.Current(t0); err != nil {
		t.Errorf("空库: %v", err)
	}
	if err := writeFile(s.path, "{broken"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Current(t0); err == nil {
		t.Error("损坏的文件应报错")
	}
	// 手改过的量程收回来
	writeFile(s.path, `{"conditions":[{"id":"1","name":"x","peak":999,"days":99,"care":"躺平","onset":"2026-08-20T08:00:00+08:00"}]}`)
	f, err := s.load()
	if err != nil || f.Conditions[0].Peak != 100 || f.Conditions[0].Days != maxDays || f.Conditions[0].Care != careTough || f.Conditions[0].ProgressAt.IsZero() {
		t.Errorf("越界字段应被收回: %+v, %v", f, err)
	}
}

func strp(s string) *string { return &s }

// 登记完当轮就注入，而时钟未必走动过一格：那时进度恰好是 0，而它必须已经算作
// 「发作了」。曾经返回 0，band 给出空串，注入里就成了「着凉第 1 天，，还在往重里走」。
func TestSeverityAtZeroProgressIsOnset(t *testing.T) {
	if got := severityAt(55, 0); got <= 0 {
		t.Errorf("进度为 0 的严重度 = %d，期望已经是发作那一档", got)
	}
	if band("", severityAt(55, 0)) == "" {
		t.Error("刚发作的状况不该给出空的严重度词")
	}
	if got := severityAt(55, -0.1); got != 0 {
		t.Errorf("进度为负（还没发作）的严重度 = %d，期望 0", got)
	}
}
