package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease 是一个假的 GitHub 发布服务：一个 releases/latest 接口，
// 加上按本机平台生成的包与校验和清单。
type fakeRelease struct {
	srv     *httptest.Server
	tag     string
	payload []byte // 包内二进制的内容
	// corruptSum 改坏校验和清单，用来测校验失败的路径。
	corruptSum bool
	// noSums 不提供校验和清单。
	noSums bool
}

func newFakeRelease(t *testing.T, tag string) *fakeRelease {
	t.Helper()
	f := &fakeRelease{tag: tag, payload: []byte("假的可执行文件内容 " + tag)}
	mux := http.NewServeMux()

	archive := packFor(t, runtime.GOOS, f.payload)
	assetName := AssetName(tag, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive)

	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("请求没带 User-Agent，GitHub 会拒")
		}
		rel := Release{
			Tag: tag, Name: tag, Body: "更新说明正文",
			Assets: []Asset{{Name: assetName, Size: int64(len(archive)), URL: f.srv.URL + "/dl/" + assetName}},
		}
		if !f.noSums {
			rel.Assets = append(rel.Assets, Asset{Name: sumsAsset, URL: f.srv.URL + "/dl/" + sumsAsset})
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/dl/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/dl/"+sumsAsset, func(w http.ResponseWriter, r *http.Request) {
		hexSum := hex.EncodeToString(sum[:])
		if f.corruptSum {
			hexSum = strings.Repeat("0", 64)
		}
		fmt.Fprintf(w, "%s  %s\n", hexSum, assetName)
		fmt.Fprintf(w, "%s  wen-%s-plan9-386.tar.gz\n", strings.Repeat("a", 64), tag)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelease) client() *Client {
	return &Client{HTTP: f.srv.Client(), API: f.srv.URL, Repo: "owner/repo", UA: "wen/test"}
}

// packFor 按平台打出与发布流水线同形状的包（Windows 是 zip，其余是 tar.gz），
// 包内带一层目录、还有别的文件，与真实产物一致。
func packFor(t *testing.T, goos string, payload []byte) []byte {
	t.Helper()
	name := BinaryName(goos)
	entries := []string{"wen-v9.9.9/README.md", "wen-v9.9.9/" + name}
	var buf bytes.Buffer

	if goos == "windows" {
		zw := zip.NewWriter(&buf)
		for _, entry := range entries {
			w, err := zw.Create(entry)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(payload); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		hdr := &tar.Header{Name: entry, Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLatestFetchExtract(t *testing.T) {
	f := newFakeRelease(t, "v9.9.9")
	c := f.client()
	ctx := context.Background()

	rel, err := c.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v9.9.9" || rel.Body != "更新说明正文" {
		t.Fatalf("发布信息不符: %+v", rel)
	}

	plan, err := c.Prepare(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Asset.Name != AssetName("v9.9.9", runtime.GOOS, runtime.GOARCH) {
		t.Fatalf("挑错了包: %s", plan.Asset.Name)
	}

	work := t.TempDir()
	var lastDone int64
	archive, err := c.Fetch(ctx, plan, work, func(done, total int64) { lastDone = done })
	if err != nil {
		t.Fatal(err)
	}
	if lastDone != plan.Asset.Size {
		t.Fatalf("进度没走到底: %d/%d", lastDone, plan.Asset.Size)
	}

	bin, err := Extract("", archive, work, BinaryName(runtime.GOOS))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.payload) {
		t.Fatalf("解出来的内容不符: %q", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(bin)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("解出来的二进制没有可执行位: %v", fi.Mode())
		}
	}
}

// 校验和对不上时必须中止，且不把坏包留在安装目录里。
func TestFetchChecksumMismatch(t *testing.T) {
	f := newFakeRelease(t, "v9.9.9")
	f.corruptSum = true
	c := f.client()
	ctx := context.Background()

	rel, err := c.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.Prepare(ctx, rel)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	_, err = c.Fetch(ctx, plan, work, nil)
	if err == nil {
		t.Fatal("校验和不符却没有报错")
	}
	if !strings.Contains(err.Error(), "校验和不符") {
		t.Fatalf("错误信息没说清楚是校验问题: %v", err)
	}
	entries, _ := os.ReadDir(work)
	if len(entries) != 0 {
		t.Fatalf("坏包没有被清理: %v", entries)
	}
}

// 发布里没有校验和清单时拒绝更新，而不是跳过校验。
func TestPrepareRequiresSums(t *testing.T) {
	f := newFakeRelease(t, "v9.9.9")
	f.noSums = true
	c := f.client()
	ctx := context.Background()

	rel, err := c.Latest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prepare(ctx, rel); err == nil {
		t.Fatal("没有校验和清单却放行了")
	}
}

func TestPrepareMissingPlatform(t *testing.T) {
	rel := Release{Tag: "v9.9.9", Assets: []Asset{
		{Name: "wen-v9.9.9-plan9-386.tar.gz"},
		{Name: sumsAsset},
	}}
	c := &Client{}
	if _, err := c.Prepare(context.Background(), rel); err == nil {
		t.Fatal("发布里没有本平台的包却放行了")
	}
}

func TestLookupSum(t *testing.T) {
	list := "aaaa  wen-v1.0.0-linux-amd64.tar.gz\nbbbb *wen-v1.0.0-windows-amd64.zip\n\n坏行\n"
	if s, ok := lookupSum(list, "wen-v1.0.0-linux-amd64.tar.gz"); !ok || s != "aaaa" {
		t.Fatalf("普通行没取到: %q %v", s, ok)
	}
	if s, ok := lookupSum(list, "wen-v1.0.0-windows-amd64.zip"); !ok || s != "bbbb" {
		t.Fatalf("带 * 标记的行没取到: %q %v", s, ok)
	}
	if _, ok := lookupSum(list, "不存在"); ok {
		t.Fatal("不该找到")
	}
}

// Apply 换上去之后，原路径上必须是新内容；三个平台都要成立。
func TestApply(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, BinaryName(runtime.GOOS))
	if err := os.WriteFile(exe, []byte("旧程序"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := WorkDir(exe)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(work, BinaryName(runtime.GOOS))
	if err := os.WriteFile(newBin, []byte("新程序"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Apply("", newBin, exe); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "新程序" {
		t.Fatalf("替换后的内容不符: %q", got)
	}

	// 收尾要能把工作目录与残留的旧文件都清掉
	CleanupOld(exe)
	if _, err := os.Stat(backupPath(exe)); !os.IsNotExist(err) {
		t.Fatalf("旧文件没被清掉: %v", err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("工作目录没被清掉: %v", err)
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, BinaryName(runtime.GOOS))
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckWritable("", exe); err != nil {
		t.Fatalf("可写目录被判成不可写: %v", err)
	}
	if err := CheckWritable("", filepath.Join(dir, "没有这个目录", "wen")); err == nil {
		t.Fatal("不存在的目录该判为不可写")
	}
	// 探测文件不该留下
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("探测留下了多余文件: %v", entries)
	}
}

// 跑不起来的「新版程序」必须挡在替换之前。
func TestSmokeTestRejectsBrokenBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, BinaryName(runtime.GOOS))
	if err := os.WriteFile(bin, []byte("这不是一个可执行文件"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SmokeTest(context.Background(), "", bin, "v9.9.9"); err == nil {
		t.Fatal("跑不起来的二进制却通过了试运行")
	}
}

// 能跑起来但自报版本不符的，同样挡在替换之前。
func TestSmokeTestVersionMismatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上没有能直接 CreateProcess 起来的脚本形式")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "wen")
	script := "#!/bin/sh\necho v1.2.3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SmokeTest(context.Background(), "", bin, "v1.2.3"); err != nil {
		t.Fatalf("版本相符却没通过: %v", err)
	}
	err := SmokeTest(context.Background(), "", bin, "v9.9.9")
	if err == nil || !strings.Contains(err.Error(), "不符") {
		t.Fatalf("版本不符却通过了: %v", err)
	}
}

func TestAssetName(t *testing.T) {
	if got := AssetName("v1.0.0", "windows", "amd64"); got != "wen-v1.0.0-windows-amd64.zip" {
		t.Fatalf("Windows 包名不符: %s", got)
	}
	if got := AssetName("v1.0.0", "darwin", "arm64"); got != "wen-v1.0.0-darwin-arm64.tar.gz" {
		t.Fatalf("macOS 包名不符: %s", got)
	}
}
