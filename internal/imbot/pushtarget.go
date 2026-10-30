package imbot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// PushTargetStore 按用户记住主动推送所需的凭据，并持久化到 <StateDir>/<filename>。
//
// 微信的 context_token、企业微信的 chatid、Discord / Slack 的 DM channel_id 都是
// 这个形状：入站消息里带一次，之后主动推送要复用。文件 0600，因为凭据等同于
// 向该用户发消息的通行证；写入走临时文件再改名，进程中途退出不会留下半截 JSON。
type PushTargetStore struct {
	mu   sync.Mutex
	path string
	m    map[string]string // 用户 ID → 最近一次入站消息给的推送凭据
}

// LoadPushTargets 读取状态目录里的推送凭据。文件不存在时返回空存储，不算错误。
func LoadPushTargets(stateDir, filename string) (*PushTargetStore, error) {
	s := &PushTargetStore{path: filepath.Join(stateDir, filename), m: map[string]string{}}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.m); err != nil {
		return nil, err
	}
	if s.m == nil {
		s.m = map[string]string{}
	}
	return s, nil
}

// Set 记住某个用户最新的推送凭据。空用户或空凭据是空操作；值没变时不重写文件。
func (s *PushTargetStore) Set(userID, target string) error {
	if userID == "" || target == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[userID] == target {
		return nil
	}
	// 先更新内存：磁盘写失败时本次运行仍能用最新凭据推送；重启后从旧文件恢复，
	// 下一条入站消息会再写一次。与微信原来的 tokenStore 行为一致。
	s.m[userID] = target
	raw, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Get 返回某个用户最近一次的推送凭据；没有时返回空串。
func (s *PushTargetStore) Get(userID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[userID]
}
