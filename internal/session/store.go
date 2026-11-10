package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store 以 JSONL 文件形式管理 session：<dir>/<id>.jsonl，
// 第一行为 Meta，其后每行一条 StoredMessage。
type Store struct {
	dir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // 每 session 一把锁，防并发写
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	return &Store{dir: dir, locks: map[string]*sync.Mutex{}}, nil
}

func (s *Store) lock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	return l
}

func (s *Store) path(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", fmt.Errorf("invalid session id %q", id)
	}
	return filepath.Join(s.dir, id+".jsonl"), nil
}

// Create 新建 session，返回其 Meta。ID 为时间戳+随机后缀，天然按时间排序。
func (s *Store) Create() (Meta, error) {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return Meta{}, err
	}
	now := time.Now()
	meta := Meta{
		Type:      "meta",
		ID:        now.Format("20060102-150405") + "-" + hex.EncodeToString(buf),
		CreatedAt: now,
	}
	p, err := s.path(meta.ID)
	if err != nil {
		return Meta{}, err
	}
	line, _ := json.Marshal(meta)
	if err := os.WriteFile(p, append(line, '\n'), 0o644); err != nil {
		return Meta{}, fmt.Errorf("create session: %w", err)
	}
	return meta, nil
}

// List 返回全部 session 的 Meta，按最近活跃倒序（新的在前）：最近一次真人交互
// 的时间，旧会话没有该字段时回落创建时间；同一时刻再按 id 倒序，使顺序稳定。
// 只认真人交互而不看最后一次写入——心跳在某个会话里自言自语，不该把它顶到最前。
func (s *Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	metas := make([]Meta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		m, err := s.readMeta(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue // 跳过损坏文件，不让单个坏文件拖垮列表
		}
		metas = append(metas, m)
	}
	sort.Slice(metas, func(i, j int) bool {
		ai, aj := metas[i].activeAt(), metas[j].activeAt()
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		return metas[i].ID > metas[j].ID
	})
	return metas, nil
}

func (s *Store) readMeta(p string) (Meta, error) {
	f, err := os.Open(p)
	if err != nil {
		return Meta{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() {
		return Meta{}, fmt.Errorf("empty session file %s", p)
	}
	var m Meta
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil || m.Type != "meta" {
		return Meta{}, fmt.Errorf("bad meta line in %s", p)
	}
	return m, nil
}

// Get 返回指定 session 的 Meta 与全部消息。
func (s *Store) Get(id string) (Meta, []StoredMessage, error) {
	p, err := s.path(id)
	if err != nil {
		return Meta{}, nil, err
	}
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()

	f, err := os.Open(p)
	if err != nil {
		return Meta{}, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var meta Meta
	var msgs []StoredMessage
	first := true
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if first {
			first = false
			if err := json.Unmarshal(line, &meta); err != nil {
				return Meta{}, nil, fmt.Errorf("bad meta line: %w", err)
			}
			continue
		}
		var m StoredMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue // 跳过损坏行
		}
		msgs = append(msgs, m)
	}
	return meta, msgs, sc.Err()
}

// Append 追加一条消息。
func (s *Store) Append(id string, msg StoredMessage) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()

	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("session %s: %w", id, err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// PutImage 把一张聊天图片写进该会话的附件目录，返回可安全放进消息的名字。
// 图片不进 JSONL：会话行保持轻量，读取历史时只按需加载。
func (s *Store) PutImage(id, mediaType string, data []byte) (string, error) {
	if _, err := s.path(id); err != nil {
		return "", err
	}
	ext := imageExt(mediaType)
	if ext == "" {
		return "", fmt.Errorf("unsupported image media type %q", mediaType)
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	name := hex.EncodeToString(buf[:]) + ext
	dir := filepath.Join(s.dir, id, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

// ImagePath 返回会话图片的落盘路径。名字只允许 PutImage 生成的形式，
// 防止 API 里带路径穿越。
func (s *Store) ImagePath(id, name string) (string, error) {
	if _, err := s.path(id); err != nil {
		return "", err
	}
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("invalid image name %q", name)
	}
	return filepath.Join(s.dir, id, "images", name), nil
}

// ReadImage 读取会话图片的原始字节。
func (s *Store) ReadImage(id, name string) ([]byte, error) {
	p, err := s.ImagePath(id, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func imageExt(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ""
}

// SetTitle 更新 meta 行的标题。
func (s *Store) SetTitle(id, title string) error {
	return s.updateMeta(id, func(m *Meta) { m.Title = title })
}

// SetTitleTagged 更新标题并记下它所属的可见域标签。
// 标题取自首条用户消息，不带归属的话它会成为绕过可见域的旁路。
func (s *Store) SetTitleTagged(id, title, tag string) error {
	return s.updateMeta(id, func(m *Meta) {
		m.Title = title
		m.Tag = tag
	})
}

// SetUsage 更新 meta 行记录的最近一次实测 token 用量（nil 表示清除）。
func (s *Store) SetUsage(id string, u *Usage) error {
	return s.updateMeta(id, func(m *Meta) { m.LastUsage = u })
}

// SetLastActive 更新 meta 行记录的最近真人交互时间。
func (s *Store) SetLastActive(id string, t time.Time) error {
	return s.updateMeta(id, func(m *Meta) { m.LastActiveAt = &t })
}

// updateMeta 读取并修改 meta 行后原子回写，消息内容不动。
func (s *Store) updateMeta(id string, mutate func(*Meta)) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()

	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	idx := strings.IndexByte(string(raw), '\n')
	if idx < 0 {
		idx = len(raw)
	}
	var meta Meta
	if err := json.Unmarshal(raw[:idx], &meta); err != nil {
		return fmt.Errorf("bad meta line: %w", err)
	}
	mutate(&meta)
	line, _ := json.Marshal(meta)

	tmp := p + ".tmp"
	out := append(line, raw[idx:]...)
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Replace 用给定消息整体替换 session 内容（meta 行保留，原子写入）。
func (s *Store) Replace(id string, msgs []StoredMessage) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(p)
	if err != nil {
		return err
	}
	var buf []byte
	line, _ := json.Marshal(meta)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	for _, m := range msgs {
		line, err := json.Marshal(m)
		if err != nil {
			return err
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Delete 删除 session 文件。
func (s *Store) Delete(id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	if err := os.Remove(p); err != nil {
		return err
	}
	// 会话附件与正文同生命周期；删除会话时一并不能留下孤儿图片。
	_ = os.RemoveAll(filepath.Join(s.dir, id))
	s.mu.Lock()
	delete(s.locks, id)
	s.mu.Unlock()
	return nil
}
