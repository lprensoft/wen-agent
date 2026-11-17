package websearch

import (
	"testing"
	"time"
)

// 这三个方法都以 now 为参数，因此不用真等：时间在测试里是普通的值。
func TestBookCacheExpires(t *testing.T) {
	b := newBook(time.Minute, 4)
	t0 := time.Now()
	b.store("k", "out", t0)

	if out, at, ok := b.lookup("k", t0.Add(30*time.Second)); !ok || out != "out" || !at.Equal(t0) {
		t.Fatalf("a fresh entry should hit: out=%q at=%v ok=%v", out, at, ok)
	}
	if _, _, ok := b.lookup("k", t0.Add(time.Minute)); ok {
		t.Error("an entry at exactly the TTL should be gone")
	}
}

func TestBookEvictsOldest(t *testing.T) {
	b := newBook(time.Hour, 2)
	t0 := time.Now()
	b.store("a", "1", t0)
	b.store("b", "2", t0.Add(time.Second))
	b.store("c", "3", t0.Add(2*time.Second))

	if _, _, ok := b.lookup("a", t0.Add(2*time.Second)); ok {
		t.Error("the oldest entry should have been evicted")
	}
	if _, _, ok := b.lookup("c", t0.Add(2*time.Second)); !ok {
		t.Error("the newest entry should survive")
	}
}

func TestBookWindow(t *testing.T) {
	b := newBook(time.Hour, 8)
	t0 := time.Now()
	if !b.allow(2, t0) {
		t.Fatal("an empty window should allow")
	}
	// 放行不记账：连放三次都该过，直到 record 把它们填满
	if !b.allow(2, t0) || !b.allow(2, t0) {
		t.Fatal("allow must not spend the quota by itself")
	}
	b.record(t0)
	b.record(t0)
	if b.allow(2, t0.Add(time.Minute)) {
		t.Error("two records against a cap of two should refuse the third")
	}
	if !b.allow(2, t0.Add(61*time.Minute)) {
		t.Error("the window should have rolled over after an hour")
	}
}
