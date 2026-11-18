package health

import (
	"testing"
	"time"

	"wen/internal/cue"
	"wen/internal/plugin"
)

func healthEditReq(op, row string, values map[string]string) plugin.StateEditRequest {
	return plugin.StateEditRequest{Lang: "zh", Domain: "", Section: stateSection, Row: row, Op: op, Values: values}
}

func seedCondition(t *testing.T, p *Plugin, now time.Time) string {
	t.Helper()
	store := p.storeFor("")
	if _, err := store.Add("感冒", sevModerate.ZH(), now, 3, careTough, now, p.snapshot().limits()); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	return snap.Active[0].Cond.ID
}

func TestEditConditionFromState(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	id := seedCondition(t, p, now)

	if err := p.EditState(healthEditReq(stateOpEdit, id, map[string]string{
		"severity": sevMild.ZH(), "care": careMeds,
	})); err != nil {
		t.Fatalf("调整 = %v", err)
	}
	snap, err := p.storeFor("").Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	if snap.Active[0].Cond.Care != careMeds {
		t.Errorf("处理方式 = %q", snap.Active[0].Cond.Care)
	}
	// 旧客户端原样回传它看到的当前档位：只改处理方式不该顺手改变病程。
	if snap.Active[0].Cond.Peak != 55 {
		t.Errorf("只改处理方式却改了峰值：%d", snap.Active[0].Cond.Peak)
	}
}

// 峰值被「严重度上限」截在规范值之间时，只改处理方式不能把它改回按档位反推的值。
func TestCareOnlyEditKeepsCappedPeak(t *testing.T) {
	p := newTestPlugin(t, nil) // 默认封顶在「难受」档
	now := time.Now()
	store := p.storeFor("")
	if _, err := store.Add("重感冒", sevSevere.ZH(), now, 3, careTough, now, p.snapshot().limits()); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	cond := snap.Active[0].Cond
	if cond.Peak >= 70 {
		t.Fatalf("封顶没有生效：peak = %d", cond.Peak)
	}

	if err := p.EditState(healthEditReq(stateOpEdit, cond.ID, map[string]string{
		"severity": severityWord(cond.Peak), "care": careMeds,
	})); err != nil {
		t.Fatalf("只改处理方式 = %v", err)
	}
	snap, err = store.Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	if snap.Active[0].Cond.Peak != cond.Peak {
		t.Errorf("峰值被顺手改掉了：%d → %d", cond.Peak, snap.Active[0].Cond.Peak)
	}
	if snap.Active[0].Cond.Care != careMeds {
		t.Errorf("处理方式没有改到：%q", snap.Active[0].Cond.Care)
	}
}

func TestRecoverConditionFromState(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	id := seedCondition(t, p, now)

	if err := p.EditState(healthEditReq(stateOpRecover, id, nil)); err != nil {
		t.Fatalf("痊愈 = %v", err)
	}
	snap, err := p.storeFor("").Current(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Active) != 0 {
		t.Fatalf("痊愈后仍有状况：%+v", snap.Active)
	}
}

// 降档后表单要显示此刻的严重度，而不是历史峰值反推出来的那一档。
func TestFormShowsCurrentSeverityAfterDowngrade(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	store := p.storeFor("")
	// 先推到峰值附近，再降档：存储层会保留历史峰值，只把进度拨到新档。
	if _, err := store.Add("重感冒", sevModerate.ZH(), now, 3, careTough, now, p.snapshot().limits()); err != nil {
		t.Fatal(err)
	}
	mod := sevModerate.ZH()
	if _, _, err := store.Apply("重感冒", Update{Severity: &mod}, now, p.snapshot().limits()); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	cond := snap.Active[0].Cond
	if snap.Active[0].Severity <= 30 {
		t.Fatalf("此刻的严重度太低，测不出降档：%d", snap.Active[0].Severity)
	}
	mild := sevMild.ZH()
	if _, _, err := store.Apply(cond.Name, Update{Severity: &mild}, now, p.snapshot().limits()); err != nil {
		t.Fatal(err)
	}
	after, err := store.Current(now)
	if err != nil || len(after.Active) != 1 {
		t.Fatalf("Current = %+v / %v", after, err)
	}
	if after.Active[0].Cond.Peak != cond.Peak {
		t.Fatalf("降档不该改历史峰值：%d → %d", cond.Peak, after.Active[0].Cond.Peak)
	}

	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || len(secs[0].Rows) != 1 || len(secs[0].Rows[0].Edits) == 0 {
		t.Fatalf("State = %+v", secs)
	}
	fields := secs[0].Rows[0].Edits[0].Fields
	if len(fields) == 0 || fields[0].Value != sevMild.ZH() {
		t.Fatalf("严重度回显 = %+v，期望 %q", fields, sevMild.ZH())
	}

	// 只改处理方式：严重度这一项原样带回，不应再写一次。
	if err := p.EditState(plugin.StateEditRequest{
		Lang: "zh", Section: stateSection, Row: cond.ID, Op: stateOpEdit,
		Values:    map[string]string{"severity": sevMild.ZH(), "care": careMeds},
		Originals: map[string]string{"severity": sevMild.ZH(), "care": careTough},
	}); err != nil {
		t.Fatalf("只改处理方式 = %v", err)
	}
	snap, err = store.Current(now)
	if err != nil || len(snap.Active) != 1 {
		t.Fatalf("Current = %+v / %v", snap, err)
	}
	if snap.Active[0].Cond.Peak != cond.Peak {
		t.Errorf("历史峰值被改掉了：%d → %d", cond.Peak, snap.Active[0].Cond.Peak)
	}
	if snap.Active[0].Severity >= 40 {
		t.Errorf("当前严重度被抬回去了：%d", snap.Active[0].Severity)
	}
}

// 标记痊愈要撤掉还没被心跳领取的发作提醒。
func TestRecoverDropsPendingCue(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	id := seedCondition(t, p, now)
	cue.Reset()
	defer cue.Reset()
	cue.Post(cue.Cue{
		Source: cueSource, Key: cueKey("", id), Text: "感冒发作了",
		Expire: now.Add(time.Hour),
	})

	if err := p.EditState(healthEditReq(stateOpRecover, id, nil)); err != nil {
		t.Fatalf("痊愈 = %v", err)
	}
	if got := cue.Take(now); len(got) != 0 {
		t.Fatalf("痊愈后仍有发作提醒：%+v", got)
	}
}

// 表单档位与病程注入用同一套 35/65 分档。
func TestViewSeverityUsesDiseaseBands(t *testing.T) {
	cases := []struct {
		v    int
		want string
	}{
		{38, sevModerate.ZH()},
		{68, sevSevere.ZH()},
		{30, sevMild.ZH()},
		{60, sevModerate.ZH()},
	}
	for _, c := range cases {
		got := viewSeverityWord(View{Phase: phaseRising, Severity: c.v})
		if got != c.want {
			t.Errorf("severity %d = %q，期望 %q", c.v, got, c.want)
		}
	}
}

func TestHealthEditRejectsBadInput(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Now()
	id := seedCondition(t, p, now)

	if err := p.EditState(healthEditReq(stateOpEdit, id, map[string]string{
		"severity": "不存在的程度", "care": careMeds,
	})); err == nil {
		t.Error("未知严重度应拒绝")
	}
	if err := p.EditState(healthEditReq(stateOpEdit, id, map[string]string{
		"severity": sevMild.ZH(), "care": "不存在的处理",
	})); err == nil {
		t.Error("未知处理方式应拒绝")
	}
	if err := p.EditState(healthEditReq(stateOpEdit, "nope", map[string]string{
		"severity": sevMild.ZH(), "care": careMeds,
	})); err == nil {
		t.Error("不存在的状况应拒绝")
	}
	req := healthEditReq(stateOpRecover, id, nil)
	req.Domain = "nope"
	if err := p.EditState(req); err == nil {
		t.Error("磁盘上不存在的域应拒绝")
	}
}
