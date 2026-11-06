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

	entries, stats, err := collect(baseDir, opt, absOut)
	if err != nil {
		return Manifest{}, Stats{}, err
	}
	if len(entries) == 0 {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.T(lang, "backup.err.nothing", "配置目录里没有可备份的内容"))
	}

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
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.output_dir", "创建备份目录失败：%v", err))
	}
	tmp, err := os.CreateTemp(filepath.Dir(absOut), ".wen-export-*")
	if err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.temp", "创建临时文件失败：%v", err))
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
		return Manifest{}, Stats{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	if err := tmp.Close(); err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.write", "写入备份失败：%v", err))
	}
	if opt.Overwrite {
		_ = os.Remove(absOut)
	}
	if err := os.Rename(tmpName, absOut); err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.rename", "保存备份文件失败：%v", err))
	}
	committed = true
	return manifest, stats, nil
}

// collect 枚举要备份的文件。它只认程序已知的路径，不遍历整个配置目录。
func collect(baseDir string, opt Options, excludeAbs string) ([]fileEntry, Stats, error) {
	lang := opt.Lang
	var stats Stats
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
	for _, name := range []string{"models.json", "plugins.state.json", "auth.json"} {
		if err := add(filepath.Join(baseDir, name), name); err != nil {
			return nil, stats, err
		}
	}

	dirs := []string{"plugins", "bundles"}
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
		if d.IsDir() {
			return nil
		}
		fullRel, err := filepath.Rel(baseDir, p)
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.read", "读取文件失败：%v", err))
		}
		if fullRel == "wen.lock" {
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
