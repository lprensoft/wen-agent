package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"wen/internal/agentplugin"
	"wen/internal/i18n"
)

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func baseFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	write(t, filepath.Join(base, "config.yaml"), "lang: zh\n", 0o644)
	write(t, filepath.Join(base, "models.json"), `{"version":1,"providers":[],"current":{}}`, 0o600)
	write(t, filepath.Join(base, "plugins.state.json"), `{"memory":{"enabled":true,"config":{"scope":"global"}}}`, 0o600)
	write(t, filepath.Join(base, "auth.json"), `{"password":"hash"}`, 0o600)
	write(t, filepath.Join(base, "sessions", "20260101-000000-abc.jsonl"), "{\"type\":\"meta\"}\n", 0o644)
	write(t, filepath.Join(base, "plugins", "memory", "memories", "a.md"), "hello\n", 0o644)
	write(t, filepath.Join(base, "bundles", "demo", "plugin.json"), `{"name":"demo"}`, 0o644)
	write(t, filepath.Join(base, "wen.lock"), `{"addr":"127.0.0.1:1"}`, 0o644)
	return base
}

// writeUpdateLock 造出更新操作留下的锁文件。有它存在，导出才会去取锁。
func writeUpdateLock(t *testing.T, base string) {
	t.Helper()
	path := filepath.Join(base, "bundles", ".updates", ".op.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExportWaitsForBundleUpdateLock(t *testing.T) {
	base := baseFixture(t)
	writeUpdateLock(t, base)
	unlock, locked, err := agentplugin.LockUpdates(context.Background(), i18n.LangZH, base)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("有锁文件时应当真的取到锁")
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{Lang: i18n.LangZH, ConfigFile: "config.yaml", Context: ctx}); err == nil {
		t.Fatal("更新锁被占用时备份不该开始")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("等待失败仍写出了归档：%v", err)
	}
}

// 从备份恢复出来的目录带着更新来源、却没有锁文件，还可能挂载成只读。导出不该往
// 数据源里写任何东西。
func TestExportFromSnapshotWithoutUpdateLock(t *testing.T) {
	base := baseFixture(t)
	sources := filepath.Join(base, "bundles", ".updates", "sources.json")
	if err := os.MkdirAll(filepath.Dir(sources), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sources, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{Lang: i18n.LangZH, ConfigFile: "config.yaml"}); err != nil {
		t.Fatalf("没有锁文件的快照应能直接导出：%v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "bundles", ".updates", ".op.lock")); !os.IsNotExist(err) {
		t.Fatalf("导出在数据源里建了锁文件：%v", err)
	}
}

func TestExportReleasesBundleUpdateLock(t *testing.T) {
	base := baseFixture(t)
	writeUpdateLock(t, base)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{Lang: i18n.LangZH, ConfigFile: "config.yaml"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, locked, err := agentplugin.LockUpdates(ctx, i18n.LangZH, base)
	if err != nil {
		t.Fatalf("备份没有释放更新锁：%v", err)
	}
	if !locked {
		t.Fatal("锁文件在，应当真的取到锁")
	}
	unlock()
}

// 读的过程中第一次写操作开始了（锁文件出现）：这一份归档不可信，应当丢掉重来。
func TestExportRetriesWhenWriteStartsMidRead(t *testing.T) {
	base := baseFixture(t)
	original := afterCollect
	attempts := 0
	afterCollect = func() {
		attempts++
		if attempts == 1 {
			writeUpdateLock(t, base)
		}
	}
	defer func() { afterCollect = original }()

	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{Lang: i18n.LangZH, ConfigFile: "config.yaml"}); err != nil {
		t.Fatalf("写操作出现后没有重试：%v", err)
	}
	if attempts != 2 {
		t.Fatalf("读取次数 = %d，想要 2", attempts)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("重试后没有归档：%v", err)
	}
}

// 读取过程中写操作把文件拿走、读失败：也要复查锁，确认是竞争造成的就持锁重来，
// 而不是把这个错误报给用户。
func TestExportRetriesWhenWriteBreaksRead(t *testing.T) {
	base := baseFixture(t)
	original := afterCollect
	attempts := 0
	afterCollect = func() {
		attempts++
		if attempts == 1 {
			writeUpdateLock(t, base)
			if err := os.RemoveAll(filepath.Join(base, "bundles", "demo")); err != nil {
				t.Errorf("删除测试包失败：%v", err)
			}
		}
	}
	defer func() { afterCollect = original }()

	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{Lang: i18n.LangZH, ConfigFile: "config.yaml"}); err != nil {
		t.Fatalf("竞争造成的读取失败没有重试：%v", err)
	}
	if attempts != 2 {
		t.Fatalf("读取次数 = %d，想要 2", attempts)
	}
}

// ZIP 安装的解包目录还不是插件包，备份不该把它带过去。
func TestExportSkipsInstallTempDir(t *testing.T) {
	base := baseFixture(t)
	unpack := filepath.Join(base, "bundles", ".unpack-12345")
	write(t, filepath.Join(unpack, "pkg", "plugin.json"), `{"name":"half"}`, 0o644)
	write(t, filepath.Join(unpack, "pkg", "skills", "a.md"), "x", 0o644)

	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	manifest, _, err := Export(base, out, Options{Lang: i18n.LangZH, ConfigFile: "config.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FileCount != 7 {
		t.Fatalf("解包目录混进了归档：文件数 %d，想要 7", manifest.FileCount)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	base := baseFixture(t)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")

	manifest, stats, err := Export(base, out, Options{
		Lang:       i18n.LangZH,
		AppVersion: "v1.0.0",
		ConfigFile: "config.yaml",
		SessionDir: filepath.Join(base, "sessions"),
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if manifest.Format != FormatVersion || manifest.FileCount != 7 || manifest.TotalBytes != stats.Bytes {
		t.Fatalf("unexpected manifest: %+v stats: %+v", manifest, stats)
	}
	if stats.SkippedSymlinks != 0 {
		t.Fatalf("unexpected skipped symlinks: %+v", stats)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("archive mode: %v %v", info, err)
		}
	}

	target := filepath.Join(t.TempDir(), "restored")
	got, gotStats, err := Import(out, target, i18n.LangZH)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got.AppVersion != "v1.0.0" || gotStats.Files != 7 {
		t.Fatalf("unexpected import result: %+v %+v", got, gotStats)
	}

	for _, name := range []string{
		"config.yaml",
		filepath.Join("sessions", "20260101-000000-abc.jsonl"),
		filepath.Join("plugins", "memory", "memories", "a.md"),
		filepath.Join("bundles", "demo", "plugin.json"),
	} {
		if read(t, filepath.Join(base, name)) != read(t, filepath.Join(target, name)) {
			t.Errorf("%s did not round-trip", name)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "wen.lock")); !os.IsNotExist(err) {
		t.Errorf("wen.lock should not be restored: %v", err)
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{"models.json", "plugins.state.json", "auth.json"} {
			info, err := os.Stat(filepath.Join(target, name))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %o, want 600", name, info.Mode().Perm())
			}
		}
	}
}

func TestExportBundleUpdateSourcesWithoutPending(t *testing.T) {
	base := baseFixture(t)
	write(t, filepath.Join(base, "bundles", ".updates", "sources.json"), `{"demo":{"manifest_url":"https://example.com/update.json"}}`, 0o600)
	write(t, filepath.Join(base, "bundles", ".updates", "pending-demo.json"), `{"name":"demo"}`, 0o600)
	write(t, filepath.Join(base, "bundles", ".updates", ".op.lock"), `locked`, 0o600)
	write(t, filepath.Join(base, "bundles", ".updates", "work", "demo.new", "plugin.json"), `{"name":"demo"}`, 0o644)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{Lang: i18n.LangZH, ConfigFile: "config.yaml", SessionDir: filepath.Join(base, "sessions")}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if _, _, err := Import(out, target, i18n.LangZH); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(target, "bundles", ".updates", "sources.json")) == "" {
		t.Fatal("更新来源没有随备份恢复")
	}
	for _, rel := range []string{filepath.Join("bundles", ".updates", "pending-demo.json"), filepath.Join("bundles", ".updates", "work"), filepath.Join("bundles", ".updates", ".op.lock")} {
		if _, err := os.Stat(filepath.Join(target, rel)); !os.IsNotExist(err) {
			t.Fatalf("半完成更新不该恢复：%s, %v", rel, err)
		}
	}
}

func TestExportRefusesInterruptedBundleSwap(t *testing.T) {
	base := baseFixture(t)
	work := filepath.Join(base, "bundles", ".updates", "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(base, "bundles", "demo"), filepath.Join(work, "demo.old")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "bundles", ".updates", "pending-demo.json"), `{"name":"demo"}`, 0o600)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{Lang: i18n.LangZH, ConfigFile: "config.yaml", SessionDir: filepath.Join(base, "sessions")}); err == nil {
		t.Fatal("切换中断、旧包只在 work 时不该生成缺包备份")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("失败的备份不该留下归档：%v", err)
	}
}

func TestExportRefusesExistingOutput(t *testing.T) {
	base := baseFixture(t)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	opt := Options{Lang: i18n.LangZH, ConfigFile: "config.yaml", SessionDir: filepath.Join(base, "sessions")}
	if _, _, err := Export(base, out, opt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Export(base, out, opt); err == nil {
		t.Fatal("second export should refuse to overwrite")
	}
	opt.Overwrite = true
	if _, _, err := Export(base, out, opt); err != nil {
		t.Fatalf("overwrite export: %v", err)
	}
}

func TestExportSkipsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks on Windows usually needs extra privileges")
	}
	base := baseFixture(t)
	if err := os.Symlink(filepath.Join(base, "models.json"), filepath.Join(base, "plugins", "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	_, stats, err := Export(base, out, Options{
		Lang: i18n.LangZH, ConfigFile: "config.yaml", SessionDir: filepath.Join(base, "sessions"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedSymlinks != 1 {
		t.Fatalf("SkippedSymlinks = %d, want 1", stats.SkippedSymlinks)
	}
}

func TestExportRequiresContent(t *testing.T) {
	_, _, err := Export(t.TempDir(), filepath.Join(t.TempDir(), "x.tar.gz"), Options{Lang: i18n.LangZH})
	if err == nil {
		t.Fatal("empty configuration directory should not produce a backup")
	}
}

func TestExportExcludesOutputInsideBackedUpDir(t *testing.T) {
	base := baseFixture(t)
	out := filepath.Join(base, "sessions", "backup.tar.gz")
	opt := Options{
		Lang: i18n.LangZH, ConfigFile: "config.yaml", SessionDir: filepath.Join(base, "sessions"),
	}
	first, _, err := Export(base, out, opt)
	if err != nil {
		t.Fatal(err)
	}
	opt.Overwrite = true
	second, _, err := Export(base, out, opt)
	if err != nil {
		t.Fatal(err)
	}
	if second.FileCount != first.FileCount {
		t.Fatalf("output archive was nested into the backup: %d != %d", second.FileCount, first.FileCount)
	}

	target := filepath.Join(t.TempDir(), "restored")
	if _, _, err := Import(out, target, i18n.LangZH); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "sessions", "backup.tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("backup archive should not contain itself: %v", err)
	}
}

func TestImportRejectsTruncatedGzip(t *testing.T) {
	base := baseFixture(t)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{
		Lang: i18n.LangZH, ConfigFile: "config.yaml", SessionDir: filepath.Join(base, "sessions"),
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"truncated trailer", raw[:len(raw)-8]},
		{"broken crc", func() []byte {
			b := append([]byte{}, raw...)
			b[len(b)-8] ^= 0xff
			return b
		}()},
		{"trailing data", append(append([]byte{}, raw...), 'x')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "bad.tar.gz")
			if err := os.WriteFile(p, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "restored")
			if _, _, err := Import(p, target, i18n.LangZH); err == nil {
				t.Fatal("Import should fail")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("target should not be created after a failed import: %v", err)
			}
		})
	}
}

func TestImportRequiresEmptyTarget(t *testing.T) {
	base := baseFixture(t)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{
		Lang: i18n.LangZH, ConfigFile: "config.yaml", SessionDir: filepath.Join(base, "sessions"),
	}); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "restored")
	write(t, filepath.Join(target, "keep.txt"), "keep", 0o644)
	if _, _, err := Import(out, target, i18n.LangZH); err == nil {
		t.Fatal("non-empty target should be refused")
	}
	if read(t, filepath.Join(target, "keep.txt")) != "keep" {
		t.Fatal("existing file should be untouched")
	}
}

func TestExportIncludesExtraDirsAndSkipsExternalSessions(t *testing.T) {
	base := baseFixture(t)
	extra := filepath.Join(base, "memories-project")
	write(t, filepath.Join(extra, "a.md"), "project memory\n", 0o644)
	external := t.TempDir()
	write(t, filepath.Join(external, "s.jsonl"), "external\n", 0o644)

	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{
		Lang:       i18n.LangZH,
		ConfigFile: "config.yaml",
		SessionDir: external,
		ExtraDirs:  []string{extra},
	}); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "restored")
	if _, _, err := Import(out, target, i18n.LangZH); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(target, "memories-project", "a.md")) != "project memory\n" {
		t.Fatal("extra directory was not backed up")
	}
	if _, err := os.Stat(filepath.Join(target, "sessions", "20260101-000000-abc.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("sessions dir should be skipped when session.dir points elsewhere: %v", err)
	}
}

type tarItem struct {
	name string
	body string
	typ  byte
	link string
}

func buildArchive(t *testing.T, manifest string, items []tarItem) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "malicious.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	add := func(h *tar.Header, body string) {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if manifest != "" {
		add(&tar.Header{Name: ManifestName, Mode: 0o600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}, manifest)
	}
	for _, it := range items {
		typ := it.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: it.name, Mode: 0o644, Size: int64(len(it.body)), Typeflag: typ, Linkname: it.link}
		if typ != tar.TypeReg {
			h.Size = 0
			it.body = ""
		}
		add(h, it.body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportRejectsUnsafeEntries(t *testing.T) {
	validManifest := `{"format":1,"app_version":"v1"}`
	cases := []struct {
		name     string
		manifest string
		items    []tarItem
	}{
		{"parent traversal", validManifest, []tarItem{{name: "data/../evil", body: "x"}}},
		{"absolute path", validManifest, []tarItem{{name: "/etc/passwd", body: "x"}}},
		{"backslash", validManifest, []tarItem{{name: `data/..\evil`, body: "x"}}},
		{"double slash", validManifest, []tarItem{{name: "data//evil", body: "x"}}},
		{"symlink", validManifest, []tarItem{{name: "data/link", typ: tar.TypeSymlink, link: "/etc/passwd"}}},
		{"missing manifest", "", []tarItem{{name: "data/models.json", body: "{}"}}},
		{"bad format", `{"format":999}`, []tarItem{{name: "data/models.json", body: "{}"}}},
		{"no data", validManifest, nil},
		{"invalid models json", validManifest, []tarItem{{name: "data/models.json", body: "not json"}}},
		{"count mismatch", `{"format":1,"file_count":2}`, []tarItem{{name: "data/models.json", body: "{}"}}},
		{"size mismatch", `{"format":1,"total_bytes":99}`, []tarItem{{name: "data/models.json", body: "{}"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := buildArchive(t, tc.manifest, tc.items)
			target := filepath.Join(t.TempDir(), "restored")
			if _, _, err := Import(archive, target, i18n.LangZH); err == nil {
				t.Fatal("Import should fail")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("target should not be created after a failed import: %v", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(target), "evil")); !os.IsNotExist(err) {
				t.Fatalf("traversal wrote outside the target: %v", err)
			}
		})
	}
}

func TestValidRel(t *testing.T) {
	for _, rel := range []string{"models.json", "sessions/a.jsonl", "plugins/x/y.md"} {
		if !validRel(rel) {
			t.Errorf("validRel(%q) = false", rel)
		}
	}
	for _, rel := range []string{"", "../x", "a/../../x", "/etc/passwd", `a\b`, "C:/x", "a//b", "./a", "a/."} {
		if validRel(rel) {
			t.Errorf("validRel(%q) = true", rel)
		}
	}
}

func TestImportRejectsTruncatedArchive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "truncated.tar.gz")
	write(t, p, "not a gzip", 0o600)
	if _, _, err := Import(p, filepath.Join(t.TempDir(), "restored"), i18n.LangZH); err == nil {
		t.Fatal("truncated archive should fail")
	}
}

func TestManifestIsFirstEntry(t *testing.T) {
	base := baseFixture(t)
	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if _, _, err := Export(base, out, Options{
		Lang: i18n.LangZH, ConfigFile: "config.yaml", SessionDir: filepath.Join(base, "sessions"),
	}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	hdr, err := tar.NewReader(gz).Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != ManifestName {
		t.Fatalf("first entry = %q, want %q", hdr.Name, ManifestName)
	}
}
