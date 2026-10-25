package imbot

import (
	"context"
	"sync"
	"time"
)

// previewInterval 是两次预览之间的最短间隔：模型每秒吐几十个增量，逐个转发既撞平台
// 限流，界面上也只是一阵闪烁。
const previewInterval = time.Second

// previewer 把核心送来的回复预览合并、节流后交给通道的 Preview 钩子：只留最新的一份，
// 每隔 previewInterval 发一次。送来的回调在模型输出的同步路径上，只记下文字、投个信号
// 就返回，联网在自己的 goroutine 里做。
type previewer struct {
	mu     sync.Mutex
	latest string
	dirty  bool
	wake   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
}

// startPreview 起一个预览器。stop 必须在发出最终回复**之前**调用并等它返回：否则最后
// 一次预览可能落在正式消息之后，把已经发完的回复又盖回半截。
func startPreview(ctx context.Context, send func(ctx context.Context, text string)) (feed func(string), stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	pv := &previewer{wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	go pv.run(ctx, send)
	feed = func(text string) {
		pv.mu.Lock()
		pv.latest, pv.dirty = text, true
		pv.mu.Unlock()
		select {
		case pv.wake <- struct{}{}:
		default:
		}
	}
	stop = func() {
		pv.cancel()
		<-pv.done
	}
	return feed, stop
}

func (pv *previewer) run(ctx context.Context, send func(ctx context.Context, text string)) {
	defer close(pv.done)
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-pv.wake:
		}
		if wait := previewInterval - time.Since(last); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		pv.mu.Lock()
		text, dirty := pv.latest, pv.dirty
		pv.dirty = false
		pv.mu.Unlock()
		if !dirty {
			continue
		}
		send(ctx, text)
		last = time.Now()
	}
}
