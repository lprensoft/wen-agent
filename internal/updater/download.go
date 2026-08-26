package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"wen/internal/i18n"
)

// progressInterval 是进度回调的最小间隔。进度是给人看的，每收到一块就回调一次
// 只会让界面在几十毫秒里刷上百遍。
const progressInterval = 400 * time.Millisecond

// Fetch 把计划里的包下载到 workDir，边下边算校验和，下完立即比对。
//
// 校验不过时删掉下载物再报错：留一个坏包在安装目录里，下次重试还得先判断它是不是
// 上次那个坏的。onProgress 可为 nil。
func (c *Client) Fetch(ctx context.Context, p Plan, workDir string, onProgress func(done, total int64)) (string, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", fmt.Errorf(i18n.T(c.Lang, "updater.err.mkdir", "创建下载目录失败: %w"), err)
	}
	dst := filepath.Join(workDir, p.Asset.Name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Asset.URL, nil)
	if err != nil {
		return "", err
	}
	if c.UA != "" {
		req.Header.Set("User-Agent", c.UA)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf(i18n.T(c.Lang, "updater.err.download", "下载失败: %w"), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(i18n.T(c.Lang, "updater.err.download_status", "下载失败: 返回 %s"), resp.Status)
	}

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf(i18n.T(c.Lang, "updater.err.write_download", "写入下载文件失败: %w"), err)
	}
	sum := sha256.New()
	total := p.Asset.Size
	if total <= 0 {
		total = resp.ContentLength
	}
	pw := &progressWriter{total: total, fn: onProgress}
	_, copyErr := io.Copy(io.MultiWriter(f, sum, pw), io.LimitReader(resp.Body, maxAssetSize))
	closeErr := f.Close()
	pw.flush()
	if copyErr != nil {
		os.Remove(dst)
		return "", fmt.Errorf(i18n.T(c.Lang, "updater.err.download_broken", "下载中断: %w"), copyErr)
	}
	if closeErr != nil {
		os.Remove(dst)
		return "", fmt.Errorf(i18n.T(c.Lang, "updater.err.write_download", "写入下载文件失败: %w"), closeErr)
	}

	got := hex.EncodeToString(sum.Sum(nil))
	if !strings.EqualFold(got, p.Sum) {
		os.Remove(dst)
		return "", fmt.Errorf(i18n.T(c.Lang, "updater.err.checksum", "校验和不符：期望 %s，实际 %s。下载内容与发布不一致，已中止"), short(p.Sum), short(got))
	}
	return dst, nil
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}

// progressWriter 只数字节数，按固定间隔向外报进度。
type progressWriter struct {
	total int64
	done  int64
	last  time.Time
	fn    func(done, total int64)
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.done += int64(len(b))
	if w.fn != nil && time.Since(w.last) >= progressInterval {
		w.last = time.Now()
		w.fn(w.done, w.total)
	}
	return len(b), nil
}

// flush 补一次末尾的进度，保证界面上停在 100% 而不是某个中间值。
func (w *progressWriter) flush() {
	if w.fn != nil {
		w.fn(w.done, w.total)
	}
}

// Extract 从包里取出那一个可执行文件，写到 workDir 下。
//
// 只按**文件名**匹配，落点也只由 workDir 与文件名拼出：包里的路径一概不参与，
// 于是不存在解压路径穿越的问题（包是自己发的，但这条不该依赖发布端的正确性）。
func Extract(lang, archive, workDir, binaryName string) (string, error) {
	dst := filepath.Join(workDir, binaryName)
	var err error
	switch {
	case strings.HasSuffix(archive, ".zip"):
		err = extractZip(lang, archive, dst, binaryName)
	case strings.HasSuffix(archive, ".tar.gz"):
		err = extractTarGz(lang, archive, dst, binaryName)
	default:
		err = fmt.Errorf(i18n.T(lang, "updater.err.bad_archive", "不认识的包格式: %s"), filepath.Base(archive))
	}
	if err != nil {
		os.Remove(dst)
		return "", err
	}
	return dst, nil
}

func extractZip(lang, archive, dst, name string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.open_archive", "打开压缩包失败: %w"), err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || path.Base(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf(i18n.T(lang, "updater.err.extract", "解压失败: %w"), err)
		}
		defer rc.Close()
		return writeExecutable(lang, dst, rc)
	}
	return fmt.Errorf(i18n.T(lang, "updater.err.no_binary", "包里没有 %s"), name)
}

func extractTarGz(lang, archive, dst, name string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.open_archive", "打开压缩包失败: %w"), err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.extract", "解压失败: %w"), err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf(i18n.T(lang, "updater.err.extract", "解压失败: %w"), err)
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != name {
			continue
		}
		return writeExecutable(lang, dst, tr)
	}
	return fmt.Errorf(i18n.T(lang, "updater.err.no_binary", "包里没有 %s"), name)
}

// writeExecutable 落盘并置上可执行位（macOS 与 Linux 上少了它换上去也起不来）。
func writeExecutable(lang, dst string, src io.Reader) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.write", "写入失败: %w"), err)
	}
	if _, err := io.Copy(f, io.LimitReader(src, maxAssetSize)); err != nil {
		f.Close()
		return fmt.Errorf(i18n.T(lang, "updater.err.write", "写入失败: %w"), err)
	}
	// 显式 Sync：紧接着就要去执行它，内容还在缓冲里的话行为无从解释
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf(i18n.T(lang, "updater.err.write", "写入失败: %w"), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf(i18n.T(lang, "updater.err.write", "写入失败: %w"), err)
	}
	return os.Chmod(dst, 0o755)
}
