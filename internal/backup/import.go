package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"wen/internal/i18n"
)

const (
	// maxTailBytes 限制 tar 结束后继续读取 gzip 尾部时允许的缓冲，防止恶意归档
	// 用一段极长的尾部数据拖住进程。
	maxTailBytes = 1 << 20
)

// Import 把 archivePath 恢复到 targetDir。targetDir 必须不存在或为空。
func Import(archivePath, targetDir, lang string) (Manifest, Stats, error) {
	target, err := filepath.Abs(targetDir)
	if err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.target", "目标目录不可用：%v", err))
	}

	if fi, err := os.Lstat(target); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.target_file", "目标路径不是目录：%s", targetDir))
		}
		ents, err := os.ReadDir(target)
		if err != nil {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.target", "目标目录不可用：%v", err))
		}
		if len(ents) > 0 {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.target_not_empty", "目标目录不是空的：%s（请先移走其中的内容）", targetDir))
		}
	} else if !os.IsNotExist(err) {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.target", "目标目录不可用：%v", err))
	}

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.target_parent", "创建目标目录失败：%v", err))
	}
	tmp, err := os.MkdirTemp(parent, ".wen-import-*")
	if err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.temp", "创建临时文件失败：%v", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmp)
		}
	}()

	manifest, stats, err := extract(archivePath, tmp, lang)
	if err != nil {
		return Manifest{}, Stats{}, err
	}
	if err := validateData(tmp, lang); err != nil {
		return Manifest{}, Stats{}, err
	}

	if fi, err := os.Stat(target); err == nil {
		ents, err := os.ReadDir(target)
		if err != nil {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.target", "目标目录不可用：%v", err))
		}
		if len(ents) > 0 {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.target_not_empty", "目标目录不是空的：%s（请先移走其中的内容）", targetDir))
		}
		if fi.IsDir() {
			if err := os.Remove(target); err != nil {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.commit", "写入目标目录失败：%v", err))
			}
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.commit", "写入目标目录失败：%v", err))
	}
	committed = true
	return manifest, stats, nil
}

func extract(archivePath, dest, lang string) (Manifest, Stats, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.archive_open", "打开备份文件失败：%v", err))
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.archive", "备份文件损坏或格式不对：%v", err))
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var (
		manifest     Manifest
		stats        Stats
		seenManifest bool
		total        int64
		count        int
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.archive", "备份文件损坏或格式不对：%v", err))
		}
		count++
		if count > maxEntries {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.too_many", "文件数量超过上限（%d）", maxEntries))
		}

		name := hdr.Name
		if name == ManifestName {
			if seenManifest {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.T(lang, "backup.err.manifest_dup", "备份里有重复的 manifest.json"))
			}
			if hdr.Size > maxManifestBytes {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.T(lang, "backup.err.manifest_big", "manifest.json 过大"))
			}
			raw, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
			if err != nil {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.archive", "备份文件损坏或格式不对：%v", err))
			}
			if len(raw) > maxManifestBytes {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.T(lang, "backup.err.manifest_big", "manifest.json 过大"))
			}
			if err := json.Unmarshal(raw, &manifest); err != nil {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.manifest_json", "manifest.json 不是合法的 JSON：%v", err))
			}
			if manifest.Format < 1 || manifest.Format > FormatVersion {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.format", "不支持的备份格式 %d（本程序支持 %d）", manifest.Format, FormatVersion))
			}
			seenManifest = true
			continue
		}

		if hdr.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !strings.HasPrefix(name, DataPrefix) {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.entry", "备份里有不安全的路径：%s", hdr.Name))
		}
		rel := strings.TrimPrefix(name, DataPrefix)
		if !validRel(rel) {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.entry", "备份里有不安全的路径：%s", hdr.Name))
		}
		if hdr.Size < 0 {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.entry_size", "备份中有文件大小异常：%s", hdr.Name))
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.entry_type", "备份里有不支持的文件类型：%s", hdr.Name))
		}
		if hdr.Size > maxTotalBytes-total {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.too_large", "备份内容超过上限（%d 字节）", maxTotalBytes))
		}
		total += hdr.Size

		dst := filepath.Join(dest, filepath.FromSlash(rel))
		if !within(dest, dst) {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.entry", "备份里有不安全的路径：%s", hdr.Name))
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.extract", "恢复文件失败：%v", err))
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.extract", "恢复文件失败：%v", err))
			}
			mode := fs.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.extract", "恢复文件失败：%v", err))
			}
			n, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil || closeErr != nil {
				if copyErr == nil {
					copyErr = closeErr
				}
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.extract", "恢复文件失败：%v", copyErr))
			}
			if n != hdr.Size {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.entry_size", "备份中有文件大小异常：%s", hdr.Name))
			}
			stats.Files++
			stats.Bytes += n
		}
	}

	// tar.Reader 遇到结束标记就返回 EOF，此时 gzip 的尾部尚未读取；继续读到
	// gzip 的 EOF 才会校验 CRC 与长度，截断或损坏的归档因此不会静默通过。
	tail, err := io.ReadAll(io.LimitReader(gz, maxTailBytes+1))
	if err != nil {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.archive", "备份文件损坏或格式不对：%v", err))
	}
	if len(tail) > 0 {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.T(lang, "backup.err.trailing", "备份文件尾部有多余数据"))
	}

	if !seenManifest {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.T(lang, "backup.err.manifest", "备份里缺少 manifest.json"))
	}
	if stats.Files == 0 {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.T(lang, "backup.err.empty_data", "备份里没有可恢复的数据"))
	}
	if manifest.FileCount > 0 && manifest.FileCount != stats.Files {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.count", "备份里的文件数量与 manifest 不一致（%d 个，manifest 记 %d 个）", stats.Files, manifest.FileCount))
	}
	if manifest.TotalBytes > 0 && manifest.TotalBytes != stats.Bytes {
		return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.size", "备份内容大小与 manifest 不一致（%d 字节，manifest 记 %d 字节）", stats.Bytes, manifest.TotalBytes))
	}

	for _, name := range []string{"models.json", "plugins.state.json", "auth.json"} {
		p := filepath.Join(dest, name)
		if _, err := os.Lstat(p); err == nil {
			if err := os.Chmod(p, 0o600); err != nil {
				return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.extract", "恢复文件失败：%v", err))
			}
		} else if !os.IsNotExist(err) {
			return Manifest{}, Stats{}, fmt.Errorf("%s", i18n.Tf(lang, "backup.err.extract", "恢复文件失败：%v", err))
		}
	}
	return manifest, stats, nil
}

func validateData(dest, lang string) error {
	for _, name := range []string{"models.json", "plugins.state.json"} {
		p := filepath.Join(dest, name)
		raw, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.extract", "恢复文件失败：%v", err))
		}
		if !json.Valid(raw) {
			return fmt.Errorf("%s", i18n.Tf(lang, "backup.err.invalid_json", "%s 不是合法的 JSON", name))
		}
	}
	return nil
}

// validRel 只接受相对、正斜杠、且不越界的路径。
func validRel(rel string) bool {
	if rel == "" || strings.ContainsAny(rel, "\\:") || strings.HasPrefix(rel, "/") {
		return false
	}
	if path.Clean(rel) != rel {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
