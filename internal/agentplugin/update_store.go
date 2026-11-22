package agentplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"wen/internal/atomicfile"
	"wen/internal/i18n"
)

const updateDirName = ".updates"

// UpdateCandidate 是一份已经验过签名、等待用户确认的更新。
// Manifest 保存响应原文：更新时验证的是用户看到的那一份，不是另取一份「最新」。
type UpdateCandidate struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	ArchiveURL string `json:"archive_url"`
	SHA256     string `json:"sha256"`
	Manifest   []byte `json:"manifest"`
	Signature  string `json:"signature"`
	Token      string `json:"token"`
}

// UpdateSource 是一个已安装包绑定的更新来源。公钥只认证之后的更新，
// 不反向证明此前用手工目录或 zip 装进来的包来自同一个发布者。
type UpdateSource struct {
	ManifestURL string           `json:"manifest_url"`
	PublicKey   string           `json:"public_key"`
	LastCheck   time.Time        `json:"last_check,omitempty"`
	LastError   string           `json:"last_error,omitempty"`
	Candidate   *UpdateCandidate `json:"candidate,omitempty"`
}

// PendingUpdate 记录已经完整暂存、等下一次服务启动时切换的更新。
type PendingUpdate struct {
	Name        string `json:"name"`
	FromVersion string `json:"from_version"`
	FromDigest  string `json:"from_digest"`
	Version     string `json:"version"`
	ToDigest    string `json:"to_digest"`
	SHA256      string `json:"sha256"`
}

func (s *Store) updatesDir() string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, updateDirName)
}

func (s *Store) sourcesPath() string {
	if dir := s.updatesDir(); dir != "" {
		return filepath.Join(dir, "sources.json")
	}
	return ""
}

func (s *Store) pendingPath(name string) string {
	if dir := s.updatesDir(); dir != "" {
		return filepath.Join(dir, "pending-"+name+".json")
	}
	return ""
}

func (s *Store) readSources() (map[string]*UpdateSource, error) {
	out := make(map[string]*UpdateSource)
	path := s.sourcesPath()
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.sources", "读取插件包更新来源失败：%w"), err)
	}
	if out == nil {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.sources_object", "插件包更新来源文件必须是一个 JSON 对象"))
	}
	return out, nil
}

func (s *Store) writeSources(sources map[string]*UpdateSource) error {
	path := s.sourcesPath()
	if path == "" {
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.err.no_store", "没有可用的插件包目录"))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0o600)
}

func copyCandidate(in *UpdateCandidate) *UpdateCandidate {
	if in == nil {
		return nil
	}
	out := *in
	out.Manifest = append([]byte(nil), in.Manifest...)
	return &out
}

func copySource(in *UpdateSource) *UpdateSource {
	if in == nil {
		return nil
	}
	out := *in
	out.Candidate = copyCandidate(in.Candidate)
	return &out
}

// Source 返回一个包的更新来源；未绑定时返回 nil。
func (s *Store) Source(name string) (*UpdateSource, error) {
	if err := checkName(s.lang, name); err != nil {
		return nil, err
	}
	sources, err := s.readSources()
	if err != nil {
		return nil, err
	}
	return copySource(sources[name]), nil
}

// Sources 返回全部来源的副本，供后台检查与命令行枚举。
func (s *Store) Sources() (map[string]*UpdateSource, error) {
	sources, err := s.readSources()
	if err != nil {
		return nil, err
	}
	out := make(map[string]*UpdateSource, len(sources))
	for name, src := range sources {
		if src != nil {
			out[name] = copySource(src)
		}
	}
	return out, nil
}

// Track 给一个已安装包绑定更新清单与发布者公钥。
func (s *Store) Track(name, manifestURL, publicKey string) error {
	if err := checkName(s.lang, name); err != nil {
		return err
	}
	if _, err := validateHTTPSURL(s.lang, manifestURL); err != nil {
		return err
	}
	key, err := normalizePublicKey(s.lang, publicKey)
	if err != nil {
		return err
	}
	if _, err := s.currentForUpdate(name); err != nil {
		return err
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()
	unlock, err := s.acquireUpdateLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	sources, err := s.readSources()
	if err != nil {
		return err
	}
	if old := sources[name]; old != nil {
		if old.ManifestURL == manifestURL && old.PublicKey == key {
			return nil
		}
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.rebind", "这个包已经绑定过更新来源；先解除绑定，再绑定新的地址或公钥"))
	}
	sources[name] = &UpdateSource{ManifestURL: manifestURL, PublicKey: key}
	return s.writeSources(sources)
}

// Untrack 解除更新来源。已有待应用更新时必须先取消，避免换掉它的信任来源。
func (s *Store) Untrack(name string) error {
	if err := checkName(s.lang, name); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	unlock, err := s.acquireUpdateLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	pending, err := s.Pending(name)
	if err != nil {
		return err
	}
	if pending != nil {
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.untrack_pending", "这个包已有待重启应用的更新，先取消待更新再解除绑定"))
	}
	sources, err := s.readSources()
	if err != nil {
		return err
	}
	delete(sources, name)
	return s.writeSources(sources)
}

// Pending 返回一个包等待重启应用的更新；没有时返回 nil。
func (s *Store) Pending(name string) (*PendingUpdate, error) {
	if err := checkName(s.lang, name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.pendingPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var p PendingUpdate
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.pending", "读取待应用更新失败：%w"), err)
	}
	if p.Name != name {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.pending_name", "待应用更新记录的包名 %q 与文件对应的包 %q 不一致"), p.Name, name)
	}
	return &p, nil
}

func (s *Store) writePending(p *PendingUpdate) error {
	path := s.pendingPath(p.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0o600)
}
