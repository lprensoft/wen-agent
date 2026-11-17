package imbot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wen/internal/atomicfile"
)

// Binding 维护远端用户 → 当前会话 ID 的映射，持久化到 <StateDir>/sessions.json，
// 重启后同一用户继续原会话；/new 换绑到新会话。
type Binding struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

func loadBinding(stateDir string) (*Binding, error) {
	b := &Binding{path: filepath.Join(stateDir, "sessions.json"), m: map[string]string{}}
	raw, err := os.ReadFile(b.path)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &b.m); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Binding) Get(userID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.m[userID]
}

// UsersFor 反查绑定到某会话的全部用户（后台轮次落到该会话时要把结果推给谁）。
func (b *Binding) UsersFor(sessionID string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for userID, sid := range b.m {
		if sid == sessionID {
			out = append(out, userID)
		}
	}
	return out
}

// Users 返回本通道上全部跟机器人说过话的用户（映射表的键）。
// 分通道投递靠它把「唯一的那位用户」接到目标会话上，见 Core.pushTo。
func (b *Binding) Users() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return sortedKeys(b.m)
}

func (b *Binding) Set(userID, sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[userID] = sessionID
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(b.m, "", "  ")
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return atomicfile.Rename(tmp, b.path)
}

// Deduper 按消息 ID 去重：平台会因网络重试、断线重连、长轮询游标回绕等原因重复
// 推送同一条消息。窗口取 5 分钟——模型一轮回复动辄几十秒，太短的窗口挡不住重连场景。
type Deduper struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const (
	dedupWindow  = 5 * time.Minute
	dedupMaxSize = 1000
)

func NewDeduper() *Deduper { return &Deduper{seen: map[string]time.Time{}} }

// IsDuplicate 判定并记录一条消息；重复返回 true。空 ID 一律不算重复。
func (d *Deduper) IsDuplicate(msgID string) bool {
	if msgID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if len(d.seen) > dedupMaxSize {
		for id, ts := range d.seen {
			if now.Sub(ts) > dedupWindow {
				delete(d.seen, id)
			}
		}
	}
	if ts, ok := d.seen[msgID]; ok && now.Sub(ts) <= dedupWindow {
		return true
	}
	d.seen[msgID] = now
	return false
}
