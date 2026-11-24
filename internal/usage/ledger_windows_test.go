//go:build windows

package usage

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// 杀毒软件与索引器会短暂独占刚建出来的分片，表现为 OpenFile 的 "Access is denied"。
// 这里用不共享的方式把当天分片独占 20ms，Append 的重试（5/10/20/40ms）应当等到它
// 放开为止，而不是立刻报错。
func TestAppendRetriesWhileShardIsBusy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "usage")
	l := mustOpen(t, dir)
	ts := time.Date(2026, 7, 7, 12, 0, 0, 0, time.Local)
	mustAppend(t, l, Entry{TS: ts, Kind: KindTurn, Prompt: 1})
	p := filepath.Join(dir, ts.Format(dayFormat)+".jsonl")

	ptr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(ptr, syscall.GENERIC_READ, 0 /* 不共享 */, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("独占打开分片: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		syscall.CloseHandle(h)
	}()

	if err := l.Append(Entry{TS: ts, Kind: KindTurn, Prompt: 2}); err != nil {
		t.Fatalf("Append 应当重试到占用放开: %v", err)
	}
	tot, err := l.Totals(ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 2 || tot.Prompt != 3 {
		t.Fatalf("Totals = %+v, want 2 requests / prompt 3", tot)
	}
}
