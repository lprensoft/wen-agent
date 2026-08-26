package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

// decayCfg 是开启淡忘的最小配置。
func decayCfg(blur, forget int) map[string]any {
	return map[string]any{
		"decay":             true,
		"decay_blur_days":   blur,
		"decay_forget_days": forget,
		"turn_extract":      false,
	}
}

func saveDecaying(t *testing.T, s *Store, name string, decay bool) Entry {
	t.Helper()
	e, err := s.Save(Entry{
		Name:        name,
		Description: name + "的要点",
		Type:        "事实",
		Content:     "第一行细节。\n第二行细节。\n第三行细节。",
		Decay:       decay,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// backdate 把一条记忆的最后使用时间往前推若干天，用来跨过淡忘的时限。
func backdate(t *testing.T, s *Store, name string, days int) {
	t.Helper()
	e, err := s.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	e.LastUsed = time.Now().AddDate(0, 0, -days)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeLocked(e); err != nil {
		t.Fatal(err)
	}
}

func TestLastUsedRoundTrip(t *testing.T) {
	s := NewStore("", t.TempDir())
	saveDecaying(t, s, "带时间的记忆", true)
	backdate(t, s, "带时间的记忆", 40)

	e, err := s.Get("带时间的记忆")
	if err != nil {
		t.Fatal(err)
	}
	if want := 40; int(time.Since(e.LastUsed).Hours()/24) != want {
		t.Errorf("最后使用时间未如实落盘：%v", e.LastUsed)
	}
	if !e.Decay {
		t.Error("淡忘标记未落盘")
	}
}

func TestLastUsedDefaultsToUpdated(t *testing.T) {
	dir := t.TempDir()
	// 升级前的文件没有 last_used，应从最后一次改动起算而不是当作零值立刻淡忘
	raw := "---\nname: 老记忆\ndescription: 摘要\ntype: 事实\n" +
		"created: 2020-01-02T03:04:05Z\nupdated: 2021-05-06T07:08:09Z\n---\n\n正文\n"
	if err := os.WriteFile(filepath.Join(dir, "老记忆.md"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := NewStore("", dir).Get("老记忆")
	if err != nil {
		t.Fatal(err)
	}
	if !e.LastUsed.Equal(e.Updated) {
		t.Errorf("缺 last_used 时应回落到 updated：%v vs %v", e.LastUsed, e.Updated)
	}
}

func TestTouchThrottlesByDay(t *testing.T) {
	s := NewStore("", t.TempDir())
	saveDecaying(t, s, "被提及的事", true)

	now := time.Now()
	// Save 刚把 LastUsed 设成现在，同一天内不该再写盘——Store 的缓存靠目录指纹，
	// 每次提及都改写会让每一次读取都重新扫盘
	if written, err := s.Touch("被提及的事", now); err != nil || written {
		t.Errorf("同一天内不该写盘: written=%v err=%v", written, err)
	}
	if written, err := s.Touch("被提及的事", now.AddDate(0, 0, 1)); err != nil || !written {
		t.Errorf("跨天应刷新: written=%v err=%v", written, err)
	}
	e, _ := s.Get("被提及的事")
	if !sameDay(e.LastUsed, now.AddDate(0, 0, 1)) {
		t.Errorf("最后使用时间未更新：%v", e.LastUsed)
	}
}

func TestBlurCollapsesToGist(t *testing.T) {
	s := NewStore("", t.TempDir())
	saveDecaying(t, s, "会淡忘的事", true)
	orig, _ := s.Get("会淡忘的事")

	e, err := s.Blur("会淡忘的事")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.Content, "第二行细节") {
		t.Errorf("塌缩后不该还留着细节：%q", e.Content)
	}
	if !strings.Contains(e.Content, "会淡忘的事的要点") || !strings.Contains(e.Content, blurNoteZH) {
		t.Errorf("塌缩后应只剩要点并注明细节已失：%q", e.Content)
	}
	// 淡忘是时间流逝的结果而不是一次使用：改了这两个时间，这条记忆就永远等不到归档
	reread, _ := s.Get("会淡忘的事")
	if !reread.Updated.Equal(orig.Updated) || !reread.LastUsed.Equal(orig.LastUsed) {
		t.Error("塌缩不该改动更新时间与最后使用时间")
	}

	// 重复塌缩不该把注记叠加上去
	again, err := s.Blur("会淡忘的事")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(again.Content, blurNoteZH) != 1 {
		t.Errorf("重复塌缩应为幂等：%q", again.Content)
	}
}

func TestArchiveMovesOutOfIndex(t *testing.T) {
	dir := t.TempDir()
	s := NewStore("", dir)
	saveDecaying(t, s, "该忘的事", true)
	saveDecaying(t, s, "还记着的事", true)

	now := time.Now()
	if _, err := s.Archive("该忘的事", now); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.List()
	if len(entries) != 1 || entries[0].Name != "还记着的事" {
		t.Errorf("归档后不该再出现在索引里：%+v", entries)
	}
	// 自动遗忘是唯一无从察觉的不可逆动作，必须留得下来
	name := "该忘的事." + now.Format("20060102") + ".md"
	if _, err := os.Stat(filepath.Join(dir, forgottenDir, name)); err != nil {
		t.Errorf("归档文件应落在 %s 目录：%v", forgottenDir, err)
	}

	// 同一条记忆被忘掉后重新记起、再忘掉是正常路径，第二次不该覆盖第一次
	saveDecaying(t, s, "该忘的事", true)
	if _, err := s.Archive("该忘的事", now); err != nil {
		t.Fatal(err)
	}
	des, _ := os.ReadDir(filepath.Join(dir, forgottenDir))
	if len(des) != 2 {
		t.Errorf("同日再次归档应另存一份，实际 %d 个文件", len(des))
	}
}

func TestSaveKeepsDecayAndClearsBlurred(t *testing.T) {
	s := NewStore("", t.TempDir())
	saveDecaying(t, s, "会淡忘的事", true)
	if _, err := s.Blur("会淡忘的事"); err != nil {
		t.Fatal(err)
	}

	// 修订时模型未必会重复带上淡忘标记，按缺省值覆盖会把它悄悄变成永久记忆
	e, err := s.Save(Entry{
		Name: "会淡忘的事", Description: "新的要点", Type: "事实",
		Content: "重新写过的完整内容。",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Decay {
		t.Error("修订不该丢掉淡忘标记")
	}
	if e.Blurred {
		t.Error("正文是新写进来的，不该还标着已塌缩")
	}
}

func TestSweepBlursThenArchives(t *testing.T) {
	p := newPluginWithComplete(t, nil, decayCfg(30, 90))
	t.Cleanup(p.Stop)
	s := p.snapshot().store

	saveDecaying(t, s, "刚说过的事", true)
	saveDecaying(t, s, "上个月说过的事", true)
	saveDecaying(t, s, "半年前说过的事", true)
	saveDecaying(t, s, "永久记着的事", false)
	backdate(t, s, "上个月说过的事", 45)
	backdate(t, s, "半年前说过的事", 180)
	backdate(t, s, "永久记着的事", 400)

	p.runSweep(p.snapshot())

	if e, _ := s.Get("刚说过的事"); e.Blurred {
		t.Error("刚提过的记忆不该淡忘")
	}
	e, err := s.Get("上个月说过的事")
	if err != nil || !e.Blurred || strings.Contains(e.Content, "第二行细节") {
		t.Errorf("过了细节淡忘时限应只剩要点：%+v %v", e, err)
	}
	if _, err := s.Get("半年前说过的事"); err == nil {
		t.Error("过了移出时限应离开记忆库")
	}
	// 未标记淡忘的记忆永久保留，与这个开关无关
	if e, err := s.Get("永久记着的事"); err != nil || e.Blurred {
		t.Errorf("未标记淡忘的记忆不该受影响：%+v %v", e, err)
	}
}

func TestSweepSkippedWhenDecayOff(t *testing.T) {
	p := newPluginWithComplete(t, nil, map[string]any{"decay": false, "turn_extract": false})
	t.Cleanup(p.Stop)
	s := p.snapshot().store

	saveDecaying(t, s, "标了淡忘但开关是关的", true)
	backdate(t, s, "标了淡忘但开关是关的", 400)
	p.runSweep(p.snapshot())

	if _, err := s.Get("标了淡忘但开关是关的"); err != nil {
		t.Error("关闭淡忘后不该动任何记忆")
	}
}

func TestSweepCoversAllDomains(t *testing.T) {
	p := newPluginWithComplete(t, nil, decayCfg(30, 90))
	t.Cleanup(p.Stop)

	inner := p.storeFor("inner")
	saveDecaying(t, inner, "里侧该忘的事", true)
	backdate(t, inner, "里侧该忘的事", 180)

	// 清扫不产生任何对模型可见的输出，维护动作本来就该覆盖全库
	p.runSweep(p.snapshot())
	if _, err := inner.Get("里侧该忘的事"); err == nil {
		t.Error("带可见域标签的库也应被清扫")
	}
}

func TestSweepRunsOncePerDay(t *testing.T) {
	p := newPluginWithComplete(t, nil, decayCfg(30, 90))
	t.Cleanup(p.Stop)
	s := p.snapshot().store
	saveDecaying(t, s, "该忘的事", true)
	backdate(t, s, "该忘的事", 180)

	base := time.Now()
	for i := range 6 {
		p.OnTurnEnd(context.Background(), chat(i, base.Add(time.Duration(i)*time.Minute)))
	}
	waitFor(t, func() bool {
		_, err := s.Get("该忘的事")
		return err != nil
	}, "清扫应在轮次结束后跑起来")

	p.turnMu.Lock()
	last, running := p.lastSweep, p.sweeping
	p.turnMu.Unlock()
	if running || sameDay(last, time.Time{}) {
		t.Errorf("清扫应已收尾并记下日期: last=%v running=%v", last, running)
	}
}

func TestInitRejectsForgetShorterThanBlur(t *testing.T) {
	p := New()
	err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, decayCfg(90, 30))
	if err == nil {
		t.Fatal("移出时限不大于淡忘时限时应拒绝这次配置")
	}
	if !strings.Contains(err.Error(), "淡忘") {
		t.Errorf("错误信息应说清是哪两项冲突：%v", err)
	}
}

// waitFor 轮询等待条件成立，用于观察后台 goroutine 的效果。
func waitFor(t *testing.T, ok func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
