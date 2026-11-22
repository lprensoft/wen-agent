package agentplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"wen/internal/atomicfile"
	"wen/internal/i18n"
	"wen/internal/updater"
)

var (
	updateRename    = atomicfile.Rename
	updateRemoveAll = os.RemoveAll
)

func (s *Store) updateWorkDir() string {
	if dir := s.updatesDir(); dir != "" {
		return filepath.Join(dir, "work")
	}
	return ""
}

func (s *Store) stagedPath(name string) string {
	return filepath.Join(s.updateWorkDir(), name+".new")
}

func (s *Store) rollbackPath(name string) string {
	return filepath.Join(s.updateWorkDir(), name+".old")
}

// digestTree 给已安装目录算一个确定性的内容指纹。符号链接与特殊文件直接报错：
// 安装时会抹掉它们，更新前若突然出现，说明目录已经被外部改动，不该默默忽略。
func digestTree(lang, root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			fmt.Fprintf(h, "D\x00%s\x00", rel)
			return nil
		case d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular():
			return fmt.Errorf(i18n.T(lang, "plugin.bundle.update.err.special_file", "插件包里有符号链接或特殊文件：%s"), rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "F\x00%s\x00%d\x00", rel, info.Size())
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// StageUpdate 验证并完整暂存用户看到的候选；正式包与工具缓存保持不动。
// token 必须等于展示候选时的 token，避免旧窗口把后来换过的另一份更新装进去。
func (s *Store) StageUpdate(ctx context.Context, name, token string) (*PendingUpdate, error) {
	if err := checkName(s.lang, name); err != nil {
		return nil, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	unlock, err := s.acquireUpdateLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if pending, err := s.Pending(name); err != nil {
		return nil, err
	} else if pending != nil {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.pending_exists", "这个包已经有待重启应用的更新"))
	}
	sources, err := s.readSources()
	if err != nil {
		return nil, err
	}
	src := sources[name]
	if src == nil || src.Candidate == nil {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.no_candidate", "先检查更新，再确认安装候选版本"))
	}
	candidate := src.Candidate
	verified, err := s.verifyCandidate(src, candidate.Manifest, candidate.Signature)
	if err != nil {
		return nil, err
	}
	if verified.Token != candidate.Token || token == "" || token != candidate.Token || !reflect.DeepEqual(verified, candidate) {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.stale_candidate", "更新候选已经变化，请重新检查并查看后再安装"))
	}
	current, err := s.currentForUpdate(name)
	if err != nil {
		return nil, err
	}
	if candidate.Name != current.Manifest.Name || !updater.Newer(current.Manifest.Version, candidate.Version) {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.not_upgrade", "候选包与当前包名不一致，或版本已不是升级"))
	}
	fromDigest, err := digestTree(s.lang, current.Root.Path())
	if err != nil {
		return nil, err
	}

	work := s.updateWorkDir()
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	newPath := s.stagedPath(name)
	oldPath := s.rollbackPath(name)
	if _, err := os.Stat(oldPath); err == nil {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.rollback_left", "上一次更新还留有回滚目录，请先重启服务完成恢复"))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := updateRemoveAll(newPath); err != nil {
		return nil, err
	}
	zipPath := filepath.Join(work, name+".zip")
	if err := updateRemoveAll(zipPath); err != nil {
		return nil, err
	}
	defer updateRemoveAll(zipPath)
	if err := s.downloadCandidate(ctx, candidate, zipPath); err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp(work, ".unpack-")
	if err != nil {
		return nil, err
	}
	defer updateRemoveAll(tmp)
	if err := unzip(s.lang, zipPath, tmp); err != nil {
		return nil, err
	}
	staged, err := descend(s.lang, tmp)
	if err != nil {
		return nil, err
	}
	b, err := Load(s.lang, staged)
	if err != nil {
		return nil, err
	}
	if b.Manifest.Name != candidate.Name || b.Manifest.Version != candidate.Version {
		return nil, fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.archive_manifest", "ZIP 内的名称或版本与签名清单不一致：%s %s"), b.Manifest.Name, b.Manifest.Version)
	}
	toDigest, err := digestTree(s.lang, staged)
	if err != nil {
		return nil, err
	}
	if digest, err := digestTree(s.lang, current.Root.Path()); err != nil {
		return nil, err
	} else if digest != fromDigest {
		return nil, fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.current_changed", "下载期间当前插件包被改动，已取消暂存"))
	}
	if err := updateRename(staged, newPath); err != nil {
		return nil, err
	}
	pending := &PendingUpdate{
		Name: name, FromVersion: current.Manifest.Version, FromDigest: fromDigest,
		Version: candidate.Version, ToDigest: toDigest, SHA256: candidate.SHA256, Token: candidate.Token,
	}
	if err := s.writePending(pending); err != nil {
		_ = updateRemoveAll(newPath)
		return nil, err
	}
	return pending, nil
}

// CancelUpdate 取消尚未开始切换的暂存更新，正式包与工具缓存不变。
func (s *Store) CancelUpdate(name string) error {
	return s.CancelUpdateContext(context.Background(), name, "")
}

// CancelUpdateContext 与 CancelUpdate 相同，但允许调用方取消文件锁等待。
// expectedToken 非空时，只允许取消界面上实际看到的那一份 pending。
func (s *Store) CancelUpdateContext(ctx context.Context, name, expectedToken string) error {
	if err := checkName(s.lang, name); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	unlock, err := s.acquireUpdateLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	pending, err := s.Pending(name)
	if err != nil {
		return err
	}
	if pending == nil {
		return nil
	}
	if expectedToken != "" && expectedToken != pending.actionToken() {
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.cancel_stale", "待更新记录已经变化，请刷新后再取消"))
	}
	if _, err := os.Stat(s.rollbackPath(name)); err == nil {
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.cancel_started", "更新切换已经开始，不能直接取消；请重启服务完成恢复"))
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.pendingPath(name)); err != nil {
		return err
	}
	if err := updateRemoveAll(s.stagedPath(name)); err != nil {
		return err
	}
	return updateRemoveAll(filepath.Join(s.updateWorkDir(), name+".zip"))
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *Store) pendingNames() ([]string, error) {
	entries, err := os.ReadDir(s.updatesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, "pending-") && strings.HasSuffix(name, ".json") {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(name, "pending-"), ".json"))
		}
	}
	return names, nil
}

// ApplyPending 在服务真正启动、插件尚未注册前应用全部暂存更新。
// 返回的问题用于启动日志；普通坏候选不阻止其它包与主程序启动。
func (s *Store) ApplyPending() []string {
	// 没有待更新包时不碰锁文件；即使目录权限有问题，正常启动也不该被维护
	// 状态拖住。拿到锁后会再枚举一次，覆盖预检之后新暂存的记录。
	if names, err := s.pendingNames(); err != nil {
		return []string{err.Error()}
	} else if len(names) == 0 {
		return nil
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	unlock, err := s.acquireUpdateLock(context.Background())
	if err != nil {
		return []string{err.Error()}
	}
	defer unlock()
	names, err := s.pendingNames()
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	for _, name := range names {
		pending, err := s.Pending(name)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if pending == nil {
			continue
		}
		if err := s.applyOne(pending); err != nil {
			problems = append(problems, i18n.Tf(s.lang, "plugin.bundle.update.err.apply", "插件包 %s 的更新未能应用：%v", name, err))
		}
	}
	return problems
}

func (s *Store) applyOne(p *PendingUpdate) error {
	if err := checkName(s.lang, p.Name); err != nil {
		return err
	}
	formal := s.Path(p.Name)
	staged := s.stagedPath(p.Name)
	rollback := s.rollbackPath(p.Name)
	formalExists, err := pathExists(formal)
	if err != nil {
		return err
	}
	stagedExists, err := pathExists(staged)
	if err != nil {
		return err
	}
	rollbackExists, err := pathExists(rollback)
	if err != nil {
		return err
	}

	// 上次在「旧目录已移开、新目录尚未顶上」之间中断：先把旧目录放回去。
	if rollbackExists && !formalExists {
		rollbackDigest, err := digestTree(s.lang, rollback)
		if err != nil {
			return err
		}
		if rollbackDigest != p.FromDigest {
			return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.rollback_changed", "回滚目录的内容指纹与旧包记录不一致，拒绝恢复"))
		}
		if err := updateRename(rollback, formal); err != nil {
			return fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.restore", "恢复旧插件包失败，旧目录仍在 %s：%w"), rollback, err)
		}
		if !stagedExists {
			_ = os.Remove(s.pendingPath(p.Name))
		}
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.interrupted", "检测到上次切换中断，已恢复旧包"))
	}

	if formalExists {
		digest, err := digestTree(s.lang, formal)
		if err != nil {
			return err
		}
		// 新包已经就位，只是上次还没来得及清理回滚目录或 pending。
		if digest == p.ToDigest && !stagedExists {
			return s.finishApply(p)
		}
		if digest != p.FromDigest {
			return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.formal_changed", "当前包在暂存后发生变化，拒绝覆盖"))
		}
	} else {
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.formal_missing", "当前包与可恢复的旧目录都不存在"))
	}
	if rollbackExists {
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.ambiguous", "正式目录与回滚目录同时存在且状态不一致，请人工检查更新目录"))
	}
	if !stagedExists {
		_ = os.Remove(s.pendingPath(p.Name))
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.staged_missing", "暂存目录已经不存在，已保留当前包并取消这次更新"))
	}
	stagedDigest, err := digestTree(s.lang, staged)
	if err != nil {
		return err
	}
	if stagedDigest != p.ToDigest {
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.staged_changed", "暂存内容在暂存后发生变化，拒绝切换"))
	}
	stagedBundle, err := Load(s.lang, staged)
	if err != nil {
		return err
	}
	if stagedBundle.Manifest.Name != p.Name || stagedBundle.Manifest.Version != p.Version {
		return fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.staged_identity", "暂存包的名称或版本与待应用记录不一致"))
	}

	if err := updateRename(formal, rollback); err != nil {
		return err
	}
	if err := updateRename(staged, formal); err != nil {
		if restoreErr := updateRename(rollback, formal); restoreErr != nil {
			return fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.swap_restore", "新目录切换失败：%v；恢复旧目录也失败：%v（旧目录在 %s）"), err, restoreErr, rollback)
		}
		return err
	}
	installed, err := Load(s.lang, formal)
	if err == nil && (installed.Manifest.Name != p.Name || installed.Manifest.Version != p.Version) {
		err = fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.installed_identity", "切换后的包名或版本不对"))
	}
	if err == nil {
		var digest string
		digest, err = digestTree(s.lang, formal)
		if err == nil && digest != p.ToDigest {
			err = fmt.Errorf("%s", i18n.T(s.lang, "plugin.bundle.update.err.installed_digest", "切换后的内容指纹与暂存不一致"))
		}
	}
	if err != nil {
		_ = updateRename(formal, staged)
		if restoreErr := updateRename(rollback, formal); restoreErr != nil {
			return fmt.Errorf(i18n.T(s.lang, "plugin.bundle.update.err.verify_restore", "新包复验失败：%v；恢复旧目录也失败：%v"), err, restoreErr)
		}
		return err
	}
	return s.finishApply(p)
}

func (s *Store) finishApply(p *PendingUpdate) error {
	// 缓存删除失败就保留 pending：LoadTools 会据此拒绝 legacy 缓存，
	// 下次启动再重试清理，而不是让新版 MCP 拿到旧工具表。
	if err := s.DropTools(p.Name); err != nil {
		return err
	}
	if err := updateRemoveAll(s.rollbackPath(p.Name)); err != nil {
		return err
	}
	if err := os.Remove(s.pendingPath(p.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	// 已应用的候选不再继续显示为「可更新」。来源与固定公钥保留。
	sources, err := s.readSources()
	if err == nil && sources[p.Name] != nil && sources[p.Name].Candidate != nil && sources[p.Name].Candidate.Version == p.Version {
		sources[p.Name].Candidate = nil
		if err := s.writeSources(sources); err != nil {
			return err
		}
	}
	return nil
}
