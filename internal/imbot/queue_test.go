package imbot

import (
	"context"
	"testing"
	"time"
)

func TestOrderedQueueKeepsOrder(t *testing.T) {
	q := NewOrderedQueue[int]()
	q.Push(1)
	q.Push(2)
	q.Push(3)
	for want := 1; want <= 3; want++ {
		got, ok := q.Pop(context.Background())
		if !ok || got != want {
			t.Fatalf("Pop = %d, %v；想要 %d", got, ok, want)
		}
	}
}

func TestOrderedQueueWaitsForPush(t *testing.T) {
	q := NewOrderedQueue[string]()
	got := make(chan string, 1)
	go func() {
		if v, ok := q.Pop(context.Background()); ok {
			got <- v
		}
	}()
	time.Sleep(10 * time.Millisecond)
	q.Push("来了")
	select {
	case v := <-got:
		if v != "来了" {
			t.Fatalf("Pop = %q", v)
		}
	case <-time.After(time.Second):
		t.Fatal("Push 后 Pop 没有被唤醒")
	}
}

func TestOrderedQueueStopsOnCancel(t *testing.T) {
	q := NewOrderedQueue[int]()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := q.Pop(ctx); ok {
		t.Fatal("ctx 取消后 Pop 应返回 false")
	}
}
