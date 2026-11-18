package bookshelf

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

// 读过的书各占一行，进度放在 Meta 里，最近印象放在 Detail 里。
func TestStateListsProgress(t *testing.T) {
	p, shelf := newPlugin(t, nil)
	put(t, shelf, "边城.txt", "翠翠")
	put(t, shelf, "小王子.md", "狐狸")

	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	store := p.storeFor("")
	if err := store.Update(func(st *State) error {
		st.Books["边城.txt"] = &Progress{
			Offset: 50, Length: 100, Started: now.Add(-48 * time.Hour), LastRead: now.Add(-time.Hour),
			Notes: []Note{{Text: "翠翠等的那个人", At: now, Percent: 50}},
		}
		st.Books["小王子.md"] = &Progress{
			Offset: 100, Length: 100, Started: now.Add(-96 * time.Hour), LastRead: now.Add(-24 * time.Hour),
			Finished: now.Add(-24 * time.Hour),
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.NewStateQuery("en", false, now))
	if len(secs) != 1 {
		t.Fatalf("块数 = %d，期望 1", len(secs))
	}
	s := secs[0]
	if s.Title != "Reading" || s.Summary != "1 reading · 1 finished" || len(s.Rows) != 2 {
		t.Fatalf("块 = %+v", s)
	}
	byTitle := map[string]plugin.StateRow{}
	for _, r := range s.Rows {
		byTitle[r.Value] = r
	}
	if got := byTitle["边城"].Meta; got != "about 50% read" {
		t.Errorf("边城进度 = %q", got)
	}
	if got := byTitle["边城"].Detail; got != "翠翠等的那个人" {
		t.Errorf("边城印象 = %q", got)
	}
	if got := byTitle["小王子"].Meta; got != "finished" {
		t.Errorf("小王子进度 = %q", got)
	}
}

// 书架空着与「有书但没读过」给两句不同的空态。
func TestStateEmptyAndPinned(t *testing.T) {
	p, shelf := newPlugin(t, nil)
	now := time.Now()
	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空态 = %+v", secs)
	}
	put(t, shelf, "边城.txt", "翠翠")
	secs = p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("有书未读的空态 = %+v", secs)
	}
	if got := p.State(plugin.NewStateQuery("zh", true, now)); got != nil {
		t.Errorf("pinned 查询应返回 nil，得到 %+v", got)
	}
}
