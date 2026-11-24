// Package backup 把配置目录打包成可迁移的归档，并从归档恢复。
//
// 归档只包含程序自己维护的数据，不遍历整个配置目录：用户可能把 config.yaml
// 放在一个项目目录里，整个目录扫下去会把源码和 .git 一并打进去。
//
// 格式固定为 manifest.json + data/ 前缀，见 docs/backup.md。
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"wen/internal/agentplugin"
	"wen/internal/atomicfile"
	"wen/internal/i18n"
)

const (
	// FormatVersion 是归档格式的版本。导入时大于这个值的归档一律拒绝。
	FormatVersion = 1
	// ManifestName 是归档根部的清单文件。
	ManifestName = "manifest.json"
	// DataPrefix 是用户数据在归档里的前缀。
	DataPrefix = "data/"

	maxManifestBytes = 1 << 20
	maxEntries       = 200000
	maxTotalBytes    = int64(4) << 30
)

// Manifest 是归档的元数据。
type Manifest struct {
	Format     int       `json:"format"`
	AppVersion string    `json:"app_version"`
	CreatedAt  time.Time `json:"created_at"`
	SourceOS   string    `json:"source_os"`
	ConfigFile string    `json:"config_file,omitempty"`
	FileCount  int       `json:"file_count"`
	TotalBytes int64     `json:"total_bytes"`
}

// Options 是导出参数。
type Options struct {
	// Lang 决定错误与提示的语言。
	Lang string
	// AppVersion 写进 manifest，恢复时用来提示版本差异。
	AppVersion string
	// ConfigFile 是配置文件在配置目录里的文件名（不含路径）。
	ConfigFile string
	// SessionDir 是当前的会话目录，可以为空（空表示 <配置目录>/sessions）。
	SessionDir string
	// ExtraDirs 是额外要包含的目录（如按项目存放的记忆），必须在配置目录内。
	ExtraDirs []string
	// Overwrite 允许覆盖已存在的输出文件。
	Overwrite bool
	// Context 取消等待插件包更新锁；为零值时用 background。
	Context context.Context
}

// Stats 是一次导出或导入的统计。
type Stats struct {
	Files           int
	Bytes           int64
	SkippedSymlinks int
	SkippedSpecial  int
}

type fileEntry struct {
	abs string
	rel string
}

// exportAttempts 是「读取期间目录开始被写入」时最多重来的次数。第二次一定持锁，
// 所以正常情况下两遍就结束。
const exportAttempts = 3

// afterCollect 在枚举完文件、开始读内容之前调用，只给测试用来模拟「第一次写操作
// 正好在读取期间开始」。
var afterCollect = func() {}

// Export 把 baseDir 里程序维护的数据写成 out。
func Export(baseDir, out string, opt Options) (Manifest, Stats, error) {
	lang := opt.Lang
	info, err := os.Stat(baseDir)
	if err != nil || !info.IsDir() {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.base_dir", "配置目录不可用：%s", baseDir))
	}

	absOut, err := filepath.Abs(out)
	if err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.output_path", "备份路径不可用：%v", err))
	}
	if !opt.Overwrite {
		if _, err := os.Stat(absOut); err == nil {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.output_exists", "备份文件已存在：%s（加 --force 覆盖）", out))
		}
	}

	for attempt := 0; attempt < exportAttempts; attempt++ {
		manifest, stats, retry, err := exportArchive(baseDir, absOut, opt)
		if err != nil {
			return Manifest{}, Stats{}, err
		}
		if !retry {
			return manifest, stats, nil
		}
	}
	return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.T(lang, "backup.err.bundle_busy", "插件包目录正在被写入，没能取到一致的快照；请稍后重试"))
}

// exportArchive 在更新锁下收集并写出归档。retry 为 true 表示读完之后才发现更新锁
// 文件出现（此前这份目录没有写操作），临时文件已经丢弃，需要持锁重来。
//
// 从预检到包文件读完都要与启动时的目录切换互斥：否则归档可能在「旧包已移进 work、
// 新包还没顶上」的瞬间枚举 bundles，漏掉那个包却仍报告成功。
func exportArchive(baseDir, absOut string, opt Options) (Manifest, Stats, bool, error) {
	lang := opt.Lang
	ctx := opt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	unlock, locked, err := agentplugin.LockUpdates(ctx, lang, baseDir)
	if err != nil {
		return Manifest{}, Stats{}, false, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.bundle_lock", "无法取得插件包目录的更新锁，备份没有开始：%v", err))
	}
	defer unlock()

	// 没取锁时，任何读取失败都要先看一眼是不是写操作开始了：它会把文件挪走或删掉，
	// 我们读到的失败正是它造成的。那种情况下重来时持锁，比把竞争错误报给用户好。
	retryIfWriting := func(cause error) (Manifest, Stats, bool, error) {
		if !locked {
			if active, err := agentplugin.UpdatesActive(baseDir); err == nil && active {
				return Manifest{}, Stats{}, true, nil
			}
		}
		return Manifest{}, Stats{}, false, cause
	}

	entries, stats, err := collect(baseDir, opt, absOut)
	if err != nil {
		return retryIfWriting(err)
	}
	if len(entries) == 0 {
		return retryIfWriting(fmt.Errorf("%s", i18n.T(lang, "backup.err.nothing", "配置目录里没有可备份的内容")))
	}

	afterCollect()

	manifest := Manifest{
		Format:     FormatVersion,
		AppVersion: opt.AppVersion,
		CreatedAt:  time.Now(),
		SourceOS:   runtime.GOOS,
		ConfigFile: opt.ConfigFile,
		FileCount:  len(entries),
		TotalBytes: stats.Bytes,
	}

	if err := os.MkdirAll(filepath.Dir(absOut), 0o755); err != nil {
		return Manifest{}, Stats{}, false, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.output_dir", "创建备份目录失败：%v", err))
	}
	tmp, err := os.CreateTemp(filepath.Dir(absOut), ".wen-export-*")
	if err != nil {
		return Manifest{}, Stats{}, false, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.temp", "创建临时文件失败：%v", err))
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := writeArchive(tmp, entries, manifest, lang); err != nil {
		return retryIfWriting(err)
	}
	if err := tmp.Sync(); err != nil {
		return Manifest{}, Stats{}, false, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	if err := tmp.Close(); err != nil {
		return Manifest{}, Stats{}, false, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return Manifest{}, Stats{}, false, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}

	// 没有取锁时，锁文件可能出现——第一次写操作刚在这份目录上开始，读到的包可能
	// 只复制了一半。丢掉这一份，重来时就会持锁。
	if !locked {
		active, err := agentplugin.UpdatesActive(baseDir)
		if err != nil {
			return Manifest{}, Stats{}, false, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.bundle_state", "无法确认插件包目录是否正在更新：%v", err))
		}
		if active {
			return Manifest{}, Stats{}, true, nil
		}
	}

	if opt.Overwrite {
		_ = os.Remove(absOut)
	}
	if err := atomicfile.Rename(tmpName, absOut); err != nil {
		return Manifest{}, Stats{}, false, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.rename", "保存备份文件失败：%v", err))
	}
	committed = true
	return manifest, stats, false, nil
}

// collect 枚举要备份的文件。它只认程序已知的路径，不遍历整个配置目录。
func collect(baseDir string, opt Options, excludeAbs string) ([]fileEntry, Stats, error) {
	lang := opt.Lang
	var stats Stats
	if err := checkBundleUpdateRecovery(baseDir, lang); err != nil {
		return nil, stats, err
	}
	seen := map[string]bool{}
	var entries []fileEntry

	add := func(abs string, rel string) error {
		if excludeAbs != "" && samePath(abs, excludeAbs) {
			return nil
		}
		info, err := os.Lstat(abs)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.read", "读取文件失败：%v", err))
		}
		if info.Mode()&os.ModeSymlink != 0 {
			stats.SkippedSymlinks++
			return nil
		}
		if !info.Mode().IsRegular() {
			stats.SkippedSpecial++
			return nil
		}
		if seen[rel] {
			return nil
		}
		seen[rel] = true
		entries = append(entries, fileEntry{abs: abs, rel: rel})
		stats.Bytes += info.Size()
		if len(entries) > maxEntries {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.too_many", "文件数量超过上限（%d）", maxEntries))
		}
		if stats.Bytes > maxTotalBytes {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.too_large", "备份内容超过上限（%d 字节）", maxTotalBytes))
		}
		return nil
	}

	if opt.ConfigFile != "" && filepath.Base(opt.ConfigFile) == opt.ConfigFile {
		if err := add(filepath.Join(baseDir, opt.ConfigFile), opt.ConfigFile); err != nil {
			return nil, stats, err
		}
	}
	// usage.json 是用量账本的预算设置（人写的），usage/ 是按天分片的账本本身
	// （机器写的）。两者都不在下面的目录列表里另有归属，且迁移机器时历史用量与
	// 预算都是要继续用的东西——丢了就等于「这个月花了多少」从头开始。
	for _, name := range []string{"models.json", "plugins.state.json", "auth.json", "usage.json"} {
		if err := add(filepath.Join(baseDir, name), name); err != nil {
			return nil, stats, err
		}
	}

	dirs := []string{"plugins", "bundles", "usage"}
	if rel, ok := relTo(baseDir, opt.SessionDir); ok && rel != "." {
		dirs = append(dirs, rel)
	} else if !ok && opt.SessionDir == "" {
		dirs = append(dirs, "sessions")
	}
	for _, d := range opt.ExtraDirs {
		if rel, ok := relTo(baseDir, d); ok && rel != "." {
			dirs = append(dirs, rel)
		}
	}

	for _, rel := range dirs {
		if err := collectDir(baseDir, rel, seen, &entries, &stats, lang, excludeAbs); err != nil {
			return nil, stats, err
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	stats.Files = len(entries)
	return entries, stats, nil
}

// checkBundleUpdateRecovery 拒绝在目录切换尚未完成时导出。正常暂存（正式旧包
// 仍在、没有回滚目录）可以备份；一旦旧包只存在于 work，或新旧目录状态未定，
// 静默跳过 work 就会生成缺包归档。
func checkBundleUpdateRecovery(baseDir, lang string) error {
	updates := filepath.Join(baseDir, "bundles", ".updates")
	entries, err := os.ReadDir(updates)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "pending-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		bundle := strings.TrimSuffix(strings.TrimPrefix(name, "pending-"), ".json")
		_, formalErr := os.Stat(filepath.Join(baseDir, "bundles", bundle))
		_, rollbackErr := os.Stat(filepath.Join(updates, "work", bundle+".old"))
		if os.IsNotExist(formalErr) || rollbackErr == nil {
			return fmt.Errorf(i18n.Tf(lang, "backup.err.bundle_update_interrupted", "外源插件包 %s 的更新切换尚未完成，请先启动服务完成恢复，再导出备份"), bundle)
		}
		if formalErr != nil {
			return formalErr
		}
		if rollbackErr != nil && !os.IsNotExist(rollbackErr) {
			return rollbackErr
		}
	}
	return nil
}

// skippedBundleTransientPath 判断要跳过的插件包目录内容：更新用的暂存、回滚、
// pending 与操作锁（只迁移来源，不恢复半完成更新），以及 ZIP 安装的解包临时目录——
// 那是还没装好的东西，恢复到另一台机器上只是一堆散文件。
func skippedBundleTransientPath(rel string) bool {
	rel = filepath.ToSlash(rel)
	if rel == "bundles/.updates/work" || strings.HasPrefix(rel, "bundles/.updates/work/") ||
		rel == "bundles/.updates/.op.lock" || strings.HasPrefix(rel, "bundles/.updates/pending-") {
		return true
	}
	if !strings.HasPrefix(rel, "bundles/") {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(rel, "bundles/"), "/")
	return strings.HasPrefix(name, ".unpack-")
}

func collectDir(baseDir, rel string, seen map[string]bool, entries *[]fileEntry, stats *Stats, lang, excludeAbs string) error {
	root := filepath.Join(baseDir, rel)
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.read", "读取文件失败：%v", err))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		stats.SkippedSymlinks++
		return nil
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.read", "读取文件失败：%v", err))
		}
		fullRel, err := filepath.Rel(baseDir, p)
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.read", "读取文件失败：%v", err))
		}
		// 更新来源要随备份迁走；尚未提交的暂存、回滚与 pending 不带过去，
		// 否则恢复到另一台机器后可能把半完成更新当成可应用状态。
		if skippedBundleTransientPath(fullRel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if fullRel == "wen.lock" {
			return nil
		}
		// 用量账本的试写文件（usage.Probe 造的 .probe-*）：它可能正好在导出时
		// 存在，内容是空的或半截的，恢复过去只是垃圾。排除范围只限 usage 的直属
		// 文件：别的目录下同名文件可能是插件的真数据（plugins/sample/.probe-state），
		// 按名字一概跳过就会把它们丢掉。也不跳过全部点号开头的文件——
		// bundles/.updates/ 下那些同样是真数据。
		if filepath.Dir(fullRel) == "usage" && strings.HasPrefix(d.Name(), ".probe-") {
			return nil
		}
		if excludeAbs != "" && samePath(p, excludeAbs) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.read", "读取文件失败：%v", err))
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			stats.SkippedSymlinks++
			return nil
		}
		if !fi.Mode().IsRegular() {
			stats.SkippedSpecial++
			return nil
		}
		if seen[fullRel] {
			return nil
		}
		seen[fullRel] = true
		*entries = append(*entries, fileEntry{abs: p, rel: fullRel})
		stats.Bytes += fi.Size()
		if len(*entries) > maxEntries {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.too_many", "文件数量超过上限（%d）", maxEntries))
		}
		if stats.Bytes > maxTotalBytes {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.too_large", "备份内容超过上限（%d 字节）", maxTotalBytes))
		}
		return nil
	})
}

func writeArchive(w io.Writer, entries []fileEntry, m Manifest, lang string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	hdr := &tar.Header{
		Name:     ManifestName,
		Mode:     0o600,
		Size:     int64(len(raw)),
		ModTime:  m.CreatedAt,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	if _, err := tw.Write(raw); err != nil {
		return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}

	for _, e := range entries {
		info, err := os.Stat(e.abs)
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.read", "读取文件失败：%v", err))
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
		}
		h.Name = DataPrefix + filepath.ToSlash(e.rel)
		h.Mode = int64(info.Mode().Perm())
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
		}
		f, err := os.Open(e.abs)
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.read", "读取文件失败：%v", err))
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			if copyErr == nil {
				copyErr = closeErr
			}
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", copyErr))
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	return nil
}

// samePath 判断两条路径是否指向同一个位置。Windows 的路径大小写不敏感。
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	absA, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(absA), filepath.Clean(absB))
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}

// relTo 返回 p 相对 base 的路径，以及 p 是否落在 base 之内。
func relTo(base, p string) (string, bool) {
	if p == "" {
		return "", false
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", false
	}
	absP, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(absBase, absP)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
