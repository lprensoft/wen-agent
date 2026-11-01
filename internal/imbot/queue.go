package imbot

import (
	"context"
	"sync"
)

// OrderedQueue 是一个无界 FIFO：Push 不阻塞，Pop 按入队顺序取出并等待新元素。
//
// 各 IM 通道的读循环只把入站消息 Push 进来，另起一个分发协程 Pop 后处理：
// 慢回复不会把收消息与回执卡住，到达顺序也天然保持。每个通道的读循环是单协程，
// 所以 Push 的先后就是网络上的先后。
type OrderedQueue[T any] struct {
	mu     sync.Mutex
	items  []T
	notify chan struct{}
}

func NewOrderedQueue[T any]() *OrderedQueue[T] {
	return &OrderedQueue[T]{notify: make(chan struct{}, 1)}
}

// Push 入队，不阻塞。
func (q *OrderedQueue[T]) Push(item T) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Pop 取队首；队列为空时等待，ctx 取消则返回 false。
func (q *OrderedQueue[T]) Pop(ctx context.Context) (T, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			var zero T
			q.items[0] = zero // 放掉引用，别让底层数组一直攥着
			q.items = q.items[1:]
			q.mu.Unlock()
			return item, true
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
		case <-ctx.Done():
			var zero T
			return zero, false
		}
	}
}
