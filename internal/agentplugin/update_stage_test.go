package agentplugin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type stageFixture struct {
	store        *Store
	base         string
	server       *httptest.Server
	archive      []byte
	archiveCalls atomic.Int32
	candidate    *UpdateCandidate
}

func newStageFixture(t *testing.T) *stageFixture {
	t.Helper()
	base := t.TempDir()
	store := NewStore("", base)
	installUpdatableBundle(t, store, "1.2.0")
	if err := store.SaveTools("pdf-tools", "1.2.0", []ToolInfo{{Local: "old_tool"}}); err != nil {
		t.Fatal(err)
	}

	newManifest := `{
	  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
	  "name": "pdf-tools", "version": "1.3.0"
	}`
	zipPath := filepath.Join(t.TempDir(), "new.zip")
	writeZip(t, zipPath, map[string]string{
		"pkg/plugin.json":       newManifest,
		"pkg/skills/a/SKILL.md": "---\nname: a\ndescription: d\n---\nnew",
	})
	archive, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &stageFixture{store: store, base: base, archive: archive}
	var manifest []byte
	var sig string
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update.json":
			_, _ = w.Write(manifest)
		case "/update.json.sig":
			_, _ = w.Write([]byte(sig))
		case "/pkg.zip":
			f.archiveCalls.Add(1)
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	manifest = []byte(fmt.Sprintf(`{"schema_version":1,"name":"pdf-tools","version":"1.3.0","archive_url":%q,"sha256":%q}`,
		f.server.URL+"/pkg.zip", hex.EncodeToString(sum[:])))
	sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest))
	store.client = f.server.Client()
	if err := store.Track("pdf-tools", f.server.URL+"/update.json", base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	f.candidate, err = store.CheckUpdate(context.Background(), "pdf-tools")
	if err != nil || f.candidate == nil {
		t.Fatalf("准备候选失败：%+v, %v", f.candidate, err)
	}
	return f
}

func TestStageThenApplyPreservesDataAndDropsOldCache(t *testing.T) {
	f := newStageFixture(t)
	dataPath := filepath.Join(f.base, "plugins", "ext_pdf_tools", "data", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(dataPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldManifest, _ := os.ReadFile(filepath.Join(f.store.Path("pdf-tools"), ManifestFile))
	oldCache, _ := os.ReadFile(f.store.cachePath("pdf-tools"))

	pending, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token)
	if err != nil {
		t.Fatalf("暂存失败：%v", err)
	}
	if pending.Version != "1.3.0" || pending.FromVersion != "1.2.0" {
		t.Fatalf("pending 不对：%+v", pending)
	}
	gotManifest, _ := os.ReadFile(filepath.Join(f.store.Path("pdf-tools"), ManifestFile))
	gotCache, _ := os.ReadFile(f.store.cachePath("pdf-tools"))
	if string(gotManifest) != string(oldManifest) || string(gotCache) != string(oldCache) {
		t.Fatal("暂存改动了正式包或旧缓存")
	}
	if f.archiveCalls.Load() != 1 {
		t.Fatalf("ZIP 下载次数为 %d，想要 1", f.archiveCalls.Load())
	}

	if problems := f.store.ApplyPending(); len(problems) != 0 {
		t.Fatalf("应用更新失败：%v", problems)
	}
	installed, err := Load("", f.store.Path("pdf-tools"))
	if err != nil || installed.Manifest.Version != "1.3.0" {
		t.Fatalf("新版本没有就位：%+v, %v", installed, err)
	}
	body, err := os.ReadFile(filepath.Join(installed.SkillsDir, "a", "SKILL.md"))
	if err != nil || !strings.HasSuffix(string(body), "new") {
		t.Fatalf("技能没有更新：%q, %v", body, err)
	}
	if tools, err := f.store.LoadTools("pdf-tools", "1.3.0"); err != nil || len(tools) != 0 {
		t.Fatalf("旧工具缓存没有失效：%+v, %v", tools, err)
	}
	if body, _ := os.ReadFile(dataPath); string(body) != "keep" {
		t.Fatal("PLUGIN_DATA 没有保留")
	}
	if pending, _ := f.store.Pending("pdf-tools"); pending != nil {
		t.Fatal("成功后 pending 还在")
	}
	if src, _ := f.store.Source("pdf-tools"); src == nil || src.Candidate != nil {
		t.Fatalf("来源应保留、候选应清掉：%+v", src)
	}
}

func TestStageRejectsStaleTokenAndBadDigestWithoutTouchingOld(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", "stale"); err == nil {
		t.Fatal("旧 token 不该通过")
	}
	if f.archiveCalls.Load() != 0 {
		t.Fatal("token 错误还下载了 ZIP")
	}

	// 签名单仍绑定原摘要，但服务器返回了另一个文件；必须下载后由 SHA256 拦住。
	f.archive[0] ^= 0xff
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err == nil {
		t.Fatal("错误 SHA256 不该通过")
	}
	if f.archiveCalls.Load() != 1 {
		t.Fatal("这个用例必须真的走到下载后的摘要校验")
	}
	installed, err := Load("", f.store.Path("pdf-tools"))
	if err != nil || installed.Manifest.Version != "1.2.0" {
		t.Fatal("失败后旧包发生变化")
	}
	if tools, _ := f.store.LoadTools("pdf-tools", "1.2.0"); len(tools) != 1 {
		t.Fatal("失败后旧缓存发生变化")
	}
}

func TestCancelUpdateKeepsCurrentBundle(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CancelUpdate("pdf-tools"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := f.store.Pending("pdf-tools"); pending != nil {
		t.Fatal("取消后 pending 还在")
	}
	if _, err := os.Stat(f.store.stagedPath("pdf-tools")); !os.IsNotExist(err) {
		t.Fatal("取消后暂存目录还在")
	}
	installed, _ := Load("", f.store.Path("pdf-tools"))
	if installed.Manifest.Version != "1.2.0" {
		t.Fatal("取消改动了当前包")
	}
}

func TestCancelRejectsTokenFromOlderPending(t *testing.T) {
	store := NewStore("", t.TempDir())
	a := &PendingUpdate{Name: "demo", FromVersion: "1.0.0", Version: "1.1.0", Token: "A"}
	if err := store.writePending(a); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelUpdate("demo"); err != nil {
		t.Fatal(err)
	}
	b := &PendingUpdate{Name: "demo", FromVersion: "1.0.0", Version: "1.2.0", Token: "B"}
	if err := store.writePending(b); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelUpdateContext(context.Background(), "demo", a.actionToken()); err == nil {
		t.Fatal("旧窗口的 token 不该取消后来暂存的版本")
	}
	if pending, _ := store.Pending("demo"); pending == nil || pending.Version != "1.2.0" {
		t.Fatalf("后来的 pending 被误删：%+v", pending)
	}
	if err := store.CancelUpdateContext(context.Background(), "demo", b.actionToken()); err != nil {
		t.Fatalf("当前 token 取消失败：%v", err)
	}
}

func TestCancelContextRejectsMissingToken(t *testing.T) {
	store := NewStore("", t.TempDir())
	if err := store.writePending(&PendingUpdate{Name: "demo", FromVersion: "1.0.0", Version: "1.1.0", Token: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelUpdateContext(context.Background(), "demo", ""); err == nil {
		t.Fatal("空 token 不该取消界面上的 pending")
	}
	if pending, _ := store.Pending("demo"); pending == nil {
		t.Fatal("空 token 删掉了 pending")
	}
}

func TestApplyRefusesCurrentChangedAfterStaging(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.store.Path("pdf-tools"), "skills", "a", "SKILL.md")
	if err := os.WriteFile(path, []byte("user edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if problems := f.store.ApplyPending(); len(problems) != 1 {
		t.Fatalf("改动当前包后仍应用了更新：%v", problems)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "user edit" {
		t.Fatal("用户改动被覆盖")
	}
}

func TestApplyRollsBackWhenSecondRenameFails(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err != nil {
		t.Fatal(err)
	}
	original := updateRename
	updateRename = func(oldPath, newPath string) error {
		if oldPath == f.store.stagedPath("pdf-tools") && newPath == f.store.Path("pdf-tools") {
			return fmt.Errorf("injected rename failure")
		}
		return original(oldPath, newPath)
	}
	problems := f.store.ApplyPending()
	updateRename = original
	if len(problems) != 1 {
		t.Fatalf("注入失败没有报出来：%v", problems)
	}
	installed, err := Load("", f.store.Path("pdf-tools"))
	if err != nil || installed.Manifest.Version != "1.2.0" {
		t.Fatalf("旧包没有恢复：%+v, %v", installed, err)
	}
	if pending, _ := f.store.Pending("pdf-tools"); pending == nil {
		t.Fatal("回滚后 pending 不该消失")
	}
	if problems := f.store.ApplyPending(); len(problems) != 0 {
		t.Fatalf("恢复后重试仍失败：%v", problems)
	}
}

func TestApplyRecoversInterruptedSwap(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err != nil {
		t.Fatal(err)
	}
	if err := updateRename(f.store.Path("pdf-tools"), f.store.rollbackPath("pdf-tools")); err != nil {
		t.Fatal(err)
	}
	if problems := f.store.ApplyPending(); len(problems) != 1 {
		t.Fatalf("中断恢复该报告一次：%v", problems)
	}
	installed, err := Load("", f.store.Path("pdf-tools"))
	if err != nil || installed.Manifest.Version != "1.2.0" {
		t.Fatal("中断后没有先恢复旧包")
	}
	if problems := f.store.ApplyPending(); len(problems) != 0 {
		t.Fatalf("恢复后的重试失败：%v", problems)
	}
	installed, _ = Load("", f.store.Path("pdf-tools"))
	if installed.Manifest.Version != "1.3.0" {
		t.Fatal("恢复后的重试没有应用新包")
	}
}

func TestApplyRefusesChangedRollback(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err != nil {
		t.Fatal(err)
	}
	if err := updateRename(f.store.Path("pdf-tools"), f.store.rollbackPath("pdf-tools")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.store.rollbackPath("pdf-tools"), "skills", "a", "SKILL.md")
	if err := os.WriteFile(path, []byte("tampered rollback"), 0o644); err != nil {
		t.Fatal(err)
	}
	if problems := f.store.ApplyPending(); len(problems) != 1 {
		t.Fatalf("被改过的回滚目录不该恢复：%v", problems)
	}
	if _, err := os.Stat(f.store.Path("pdf-tools")); !os.IsNotExist(err) {
		t.Fatal("被改过的回滚目录被移回了正式位置")
	}
}

func TestCheckRejectsBrokenCurrentVersion(t *testing.T) {
	f := newStageFixture(t)
	manifest := `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"pdf-tools","version":"broken"}`
	if err := os.WriteFile(filepath.Join(f.store.Path("pdf-tools"), ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CheckUpdate(context.Background(), "pdf-tools"); err == nil {
		t.Fatal("当前版本损坏不该被误报为已最新")
	}
	if f.archiveCalls.Load() != 0 {
		t.Fatal("当前版本损坏还发起了更新下载")
	}
}

func TestApplyCacheDeleteFailureNeverRegistersLegacyTools(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"bundle":"pdf-tools","tools":[{"local":"legacy_tool"}]}`)
	if err := os.WriteFile(f.store.cachePath("pdf-tools"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	original := removeToolCache
	removeToolCache = func(string) error { return fmt.Errorf("injected cache delete failure") }
	problems := f.store.ApplyPending()
	removeToolCache = original
	if len(problems) != 1 {
		t.Fatalf("缓存删除失败没有报出来：%v", problems)
	}
	installed, err := Load("", f.store.Path("pdf-tools"))
	if err != nil || installed.Manifest.Version != "1.3.0" {
		t.Fatalf("这个用例需要先让新版目录就位：%+v, %v", installed, err)
	}
	if pending, _ := f.store.Pending("pdf-tools"); pending == nil {
		t.Fatal("缓存删除失败后 pending 不该被清掉")
	}
	p := NewPlugin("", installed, f.store)
	if len(p.Tools()) != 0 {
		t.Fatal("新版插件沿用了旧版 MCP 工具")
	}
	// 用户按提示重新握手，缓存已经改写成新版；重试清理不能把它删掉。
	if err := f.store.SaveTools("pdf-tools", "1.3.0", []ToolInfo{{Local: "new_tool"}}); err != nil {
		t.Fatal(err)
	}
	if problems := f.store.ApplyPending(); len(problems) != 0 {
		t.Fatalf("恢复删除后重试失败：%v", problems)
	}
	tools, err := f.store.LoadTools("pdf-tools", "1.3.0")
	if err != nil || len(tools) != 1 || tools[0].Local != "new_tool" {
		t.Fatalf("重试清理删掉了刚写下的新版缓存：%+v, %v", tools, err)
	}
}

func TestUninstallRetryFinishesCacheCleanup(t *testing.T) {
	f := newStageFixture(t)
	original := removeToolCache
	removeToolCache = func(string) error { return fmt.Errorf("injected cache delete failure") }
	err := f.store.Remove("pdf-tools")
	removeToolCache = original
	if err == nil {
		t.Fatal("缓存删除失败没有报出来")
	}
	if _, statErr := os.Stat(f.store.Path("pdf-tools")); !os.IsNotExist(statErr) {
		t.Fatal("正式目录应该已经删除")
	}
	if err := f.store.Remove("pdf-tools"); err != nil {
		t.Fatalf("重试卸载没有完成残留清理：%v", err)
	}
	if _, statErr := os.Stat(f.store.cachePath("pdf-tools")); !os.IsNotExist(statErr) {
		t.Fatalf("重试后缓存仍在：%v", statErr)
	}
	if err := f.store.Remove("never-installed"); err == nil {
		t.Fatal("没装过的包应当报错")
	}
}

func TestVersionedToolCache(t *testing.T) {
	store := NewStore("", t.TempDir())
	if err := store.SaveTools("demo", "1.0.0", []ToolInfo{{Local: "one"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.LoadTools("demo", "1.1.0"); len(got) != 0 {
		t.Fatal("别的版本读到了旧工具")
	}
	legacy := []byte(`{"bundle":"demo","tools":[{"local":"legacy"}]}`)
	if err := os.MkdirAll(filepath.Dir(store.cachePath("demo")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.cachePath("demo"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.LoadTools("demo", "9.0.0"); len(got) != 1 {
		t.Fatal("无版本 legacy 缓存不该被静默全部清掉")
	}
}

func TestPendingRejectsMismatchedName(t *testing.T) {
	store := NewStore("", t.TempDir())
	if err := os.MkdirAll(store.updatesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"name":"other","from_version":"1.0.0","version":"1.1.0"}`)
	if err := os.WriteFile(store.pendingPath("demo"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pending("demo"); err == nil {
		t.Fatal("pending 文件名与内容包名不一致不该通过")
	}
}

func TestUninstallRemovesSourceAndPending(t *testing.T) {
	f := newStageFixture(t)
	if _, err := f.store.StageUpdate(context.Background(), "pdf-tools", f.candidate.Token); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Remove("pdf-tools"); err != nil {
		t.Fatal(err)
	}
	if src, _ := f.store.Source("pdf-tools"); src != nil {
		t.Fatal("卸载后来源还在")
	}
	if _, err := os.Stat(f.store.pendingPath("pdf-tools")); !os.IsNotExist(err) {
		t.Fatal("卸载后 pending 还在")
	}
	if _, err := os.Stat(f.store.stagedPath("pdf-tools")); !os.IsNotExist(err) {
		t.Fatal("卸载后暂存目录还在")
	}
	if _, err := os.Stat(f.store.cachePath("pdf-tools")); !os.IsNotExist(err) {
		t.Fatal("卸载后工具缓存还在")
	}
}
