package agentplugin

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"wen/internal/i18n"
)

// 解包时的上限。插件包是从外面拿来的，压缩包里写多少内容不由我们决定，
// 没有上限就等于让一个几十 KB 的文件把磁盘写满（zip 炸弹）。
const (
	maxUnpackBytes = 64 << 20 // 单个包解出来的总字节数
	maxUnpackFiles = 5000     // 单个包解出来的文件数
)

// Install 从本地目录或 zip 压缩包安装一个插件包。
//
// 顺序是有讲究的：**先在原地校验，再复制**。反过来的话，一个清单写坏的包会先在
// 存放目录里落下半个残骸，下次列表时它又会作为「装了但读不出来」被报出来，用户
// 得自己去删。校验放前面，失败时存放目录一个字节都没动过。
func (s *Store) Install(src string) (*Bundle, error) {
	lang := s.lang
	if s.dir == "" {
		return nil, fmt.Errorf("%s", i18n.T(lang, "plugin.bundle.err.no_store", "没有可用的插件包目录"))
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.not_found", "找不到 %s：%w"), src, err)
	}

	// 压缩包先解到存放目录旁边的临时目录里：跨设备的 rename 会失败，而解压之后
	// 那一步正是 rename。
	stage := src
	if !info.IsDir() {
		if !strings.EqualFold(filepath.Ext(src), ".zip") {
			return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.bad_source", "只认目录与 .zip 压缩包，不认识 %s"), filepath.Base(src))
		}
		if err := os.MkdirAll(s.dir, 0o755); err != nil {
			return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.mkdir", "创建插件包目录失败：%w"), err)
		}
		tmp, err := os.MkdirTemp(s.dir, ".unpack-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		if err := unzip(lang, src, tmp); err != nil {
			return nil, err
		}
		if stage, err = descend(lang, tmp); err != nil {
			return nil, err
		}
	}

	b, err := Load(lang, stage)
	if err != nil {
		return nil, err
	}
	name := b.Manifest.Name
	dst := s.Path(name)
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.exists", "已经装过名为 %q 的插件包，先卸载再装"), name)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, fmt.Errorf(i18n.T(lang, "plugin.bundle.err.mkdir", "创建插件包目录失败：%w"), err)
	}

	if info.IsDir() {
		if err := copyTree(stage, dst); err != nil {
			os.RemoveAll(dst)
			return nil, err
		}
	} else if err := os.Rename(stage, dst); err != nil {
		// 同一个存放目录下的 rename 不该失败，真失败了就退回逐个复制
		if err := copyTree(stage, dst); err != nil {
			os.RemoveAll(dst)
			return nil, err
		}
	}

	// 从最终位置重新加载一次：复制过程中跳掉的符号链接会让内容与暂存时不同，
	// 报给用户的必须是「装完之后实际是什么样」。
	return Load(lang, dst)
}

// descend 处理「压缩包里套了一层目录」这个几乎总会发生的情况：
// GitHub 下载的 zip 解出来是 <仓库名>-<分支>/，清单在那一层里面。
// 只在顶层恰好是一个目录且顶层没有清单时才下沉一级，多于一层不猜。
func descend(lang, dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dir, entries[0].Name()), nil
	}
	return "", fmt.Errorf(i18n.T(lang, "plugin.bundle.err.zip_no_manifest", "压缩包里找不到 %s"), ManifestFile)
}

// unzip 解压到 dst。
//
// 每一条目录项都要自己拼路径再查一遍是否越界：压缩包里的名字是攻击者可控的，
// 写成 ../../ 就能落到解压目录之外（zip slip）。symlink 一律不解出来——
// 它是同一个问题的另一条路径，而插件包本来也不需要。
func unzip(lang, src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf(i18n.T(lang, "plugin.bundle.err.zip_open", "打不开压缩包：%w"), err)
	}
	defer r.Close()

	var total int64
	var count int
	for _, f := range r.File {
		if count++; count > maxUnpackFiles {
			return fmt.Errorf(i18n.T(lang, "plugin.bundle.err.zip_files", "压缩包里的文件超过 %d 个，已拒绝"), maxUnpackFiles)
		}
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf(i18n.T(lang, "plugin.bundle.err.zip_escape", "压缩包里有指向外部的路径 %q，已拒绝"), f.Name)
		}
		path := filepath.Join(dst, name)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue // 符号链接不解出来
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		n, err := copyZipFile(f, path, maxUnpackBytes-total)
		if err != nil {
			return err
		}
		if total += n; total > maxUnpackBytes {
			return fmt.Errorf(i18n.T(lang, "plugin.bundle.err.zip_bytes", "压缩包解出来超过 %d MiB，已拒绝"), maxUnpackBytes>>20)
		}
	}
	return nil
}

// copyZipFile 解出一个文件，最多写 limit 个字节。
func copyZipFile(f *zip.File, path string, limit int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	if limit < 0 {
		limit = 0
	}
	// 多读一个字节，好把「正好到上限」与「其实还没完」区分开
	return io.Copy(out, io.LimitReader(rc, limit+1))
}

// copyTree 递归复制目录，跳过符号链接。
//
// 跳过而不是跟随：跟随会把插件包目录之外的内容复制进来，而「包内的路径不得逃出
// 包外」正是标准要挡的事——装的时候就把它抹平，比装完之后每次加载都去查一遍省事。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			return nil
		case !d.Type().IsRegular():
			return nil // 设备文件、管道之类，插件包里不该有
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
