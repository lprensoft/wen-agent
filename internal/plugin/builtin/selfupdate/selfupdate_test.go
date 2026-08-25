package selfupdate

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
	"time"

	"wen/internal/plugin"
	"wen/internal/updater"
	"wen/internal/version"
)

// 比当前版本新得不可能被追上的一个版本号，用来触发「有新版」的分支。
const newTag = "v999.0.0"

// fakeGitHub 是一个假的发布服务：一次发布、一个本机平台的包、一份校验和清单。
// 包里那个「二进制」是段 shell 脚本，只会打印自己的版本号——试运行那一步要真的
// 把它跑起来，所以 Windows 上装不了（见 TestInstallAndRestart 的 skip）。
func fakeGitHub(t *testing.T, tag string) *httptest.Server {
	return fakeGitHubSums(t, tag, true)
}

// fakeGitHubSums 的 goodSum 为 false 时清单里给一个对不上的哈希。校验失败那条路径
// 必须由这里制造，不能在外面套一层代理去改写清单：发布 JSON 里的下载地址是绝对的，
// 代理转发不到它，于是「以为在测校验失败，其实一路顺利地装上了」——真发生过，
// 而且在 Windows 上还因为试运行那步先失败而看起来是通过的。
func fakeGitHubSums(t *testing.T, tag string, goodSum bool) *httptest.Server {
	t.Helper()

	payload := []byte("#!/bin/sh\necho " + tag + "\n")
	archive := packArchive(t, payload)
	assetName := updater.AssetName(tag, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive)

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     tag,
			"name":         tag,
			"body":         "### 修复\n- 修了一个问题",
			"published_at": "2026-08-01T00:00:00Z",
			"assets": []map[string]any{
				{"name": assetName, "size": len(archive), "browser_download_url": srv.URL + "/dl/pkg"},
				{"name": "SHA256SUMS.txt", "browser_download_url": srv.URL + "/dl/sums"},
			},
		})
	})
	mux.HandleFunc("/dl/pkg", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/dl/sums", func(w http.ResponseWriter, r *http.Request) {
		hexSum := hex.EncodeToString(sum[:])
		if !goodSum {
			hexSum = strings.Repeat("0", 64)
		}
		fmt.Fprintf(w, "%s  %s\n", hexSum, assetName)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func packArchive(t *testing.T, payload []byte) []byte {
	t.Helper()
	name := updater.HostBinaryName()
	var buf bytes.Buffer
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("wen-x/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "wen-x/" + name, Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newPlugin 装一个指向假服务、假可执行文件的插件；autoCheck 关掉，
// 免得后台循环插进来搅乱断言（要测它的时候单独开）。
func newPlugin(t *testing.T, srv *httptest.Server, restart RestartFunc) (*Plugin, string, string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	exeDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(exeDir, updater.HostBinaryName())
	if err := os.WriteFile(exe, []byte("旧程序"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := New(Options{Restart: restart, API: srv.URL, Repo: "owner/repo", Exe: exe})
	if err := p.Init(plugin.InitContext{StateDir: stateDir}, map[string]any{"auto_check": false}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p, exe, stateDir
}

// waitAction 等操作走到终态。
func waitAction(t *testing.T, p *Plugin) plugin.ActionState {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.ActionState(actionUpdate)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status == plugin.ActionDone || st.Status == plugin.ActionError {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("操作一直没有结束")
	return plugin.ActionState{}
}

// 第一次点只查不装：把版本号与更新说明摆出来，按钮文案随之变成「更新到 … 并重启」。
func TestCheckOnlyReportsFirst(t *testing.T) {
	srv := fakeGitHub(t, newTag)
	p, exe, stateDir := newPlugin(t, srv, nil)

	// 按钮文案固定（它要能进语言包），处境写在 Detail 里
	if got := p.Actions()[0]; got.Label != "检查并更新" || !strings.Contains(got.Detail, "已是最新版") {
		t.Fatalf("初始文案不符: %+v", got)
	}
	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}
	st := waitAction(t, p)
	if st.Status != plugin.ActionDone || !strings.Contains(st.Message, newTag) {
		t.Fatalf("检查结果不符: %+v", st)
	}
	if !strings.Contains(st.Message, "修了一个问题") {
		t.Fatalf("更新说明没带出来: %q", st.Message)
	}

	// 只查不装：程序文件一个字节都不该动
	raw, err := os.ReadFile(exe)
	if err != nil || string(raw) != "旧程序" {
		t.Fatalf("检查阶段动了程序文件: %q %v", raw, err)
	}
	// 查到的版本要落盘，重启后不必再等一个周期才知道有新版
	if got := loadState(stateDir).Latest; got != newTag {
		t.Fatalf("状态没记下最新版本: %q", got)
	}
	// 版本号在 Detail 里而不是 Label 里：Label 要能进语言包，掺了版本号就进不去
	if got := p.Actions()[0].Detail; !strings.Contains(got, newTag) {
		t.Fatalf("查到新版后该在 Detail 里点明是哪一版: %q", got)
	}
	if got := p.Actions()[0].Label; got != "检查并更新" {
		t.Fatalf("按钮文案该是固定的一句: %q", got)
	}
	if lines := p.StatusLines(); len(lines) != 1 || !strings.Contains(lines[0], newTag) {
		t.Fatalf("状态行不符: %v", lines)
	}
}

// 已是最新版时如实说，不去下载任何东西。
func TestUpToDate(t *testing.T) {
	srv := fakeGitHub(t, "v0.0.1")
	p, _, _ := newPlugin(t, srv, nil)

	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}
	st := waitAction(t, p)
	if st.Status != plugin.ActionDone || !strings.Contains(st.Message, "已是最新版") {
		t.Fatalf("结果不符: %+v", st)
	}
	if got := p.Actions()[0]; got.Label != "检查并更新" || !strings.Contains(got.Detail, "已是最新版") {
		t.Fatalf("已是最新版时该这么说: %+v", got)
	}
}

// 第二次点才真的更新：下载、校验、试运行、替换、请求重启，各步都要发生。
func TestInstallAndRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("试运行那一步要真的执行包里的文件，Windows 上没有能直接起来的脚本形式")
	}
	restartDelay = 10 * time.Millisecond
	t.Cleanup(func() { restartDelay = 1500 * time.Millisecond })

	srv := fakeGitHub(t, newTag)
	restarted := make(chan string, 1)
	p, exe, stateDir := newPlugin(t, srv, func(reason string) error {
		restarted <- reason
		return nil
	})

	// 第一次点：查。之后按钮文案变成「更新到 … 并重启」，第二次点才装
	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}
	waitAction(t, p)
	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}

	select {
	case reason := <-restarted:
		if !strings.Contains(reason, newTag) {
			t.Fatalf("重启理由里没说是哪一版: %q", reason)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("没有请求重启")
	}

	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), newTag) {
		t.Fatalf("程序文件没被换成新版: %q", raw)
	}
	// 换好了但还没重启完：状态里留着 pending，重启后由 reconcile 认领
	st := loadState(stateDir)
	if st.Pending == nil || st.Pending.To != newTag || st.Pending.From != version.Version {
		t.Fatalf("没有记下待生效的更新: %+v", st.Pending)
	}
}

// 重启不可用（如评测装配里没有那个回调）时照样更新，只是明说要自己重启。
func TestInstallWithoutRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("同 TestInstallAndRestart")
	}
	srv := fakeGitHub(t, newTag)
	p, _, _ := newPlugin(t, srv, nil)

	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}
	waitAction(t, p)
	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}
	st := waitAction(t, p)
	if st.Status != plugin.ActionDone || !strings.Contains(st.Message, "重新启动") {
		t.Fatalf("结果不符: %+v", st)
	}
}

// 校验和对不上时中止，程序文件保持原样。
func TestChecksumMismatchKeepsBinary(t *testing.T) {
	bad := fakeGitHubSums(t, newTag, false) // 清单里的哈希与包对不上
	p, exe, _ := newPlugin(t, bad, nil)
	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}
	waitAction(t, p)
	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}
	st := waitAction(t, p)
	if st.Status != plugin.ActionError {
		t.Fatalf("校验不过却没报错: %+v", st)
	}
	raw, _ := os.ReadFile(exe)
	if string(raw) != "旧程序" {
		t.Fatalf("校验失败却动了程序文件: %q", raw)
	}
}

// 重启之后：跑起来的正是那次更新的目标版本，进展窗直接定稿成「已完成」。
func TestReconcileAfterRestart(t *testing.T) {
	srv := fakeGitHub(t, newTag)
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, updater.HostBinaryName())
	if err := os.WriteFile(exe, []byte("新程序"), 0o755); err != nil {
		t.Fatal(err)
	}
	pend := state{Pending: &pendingUpdate{From: "v0.0.1", To: version.Version, At: time.Now()}}
	raw, _ := json.Marshal(pend)
	if err := os.WriteFile(filepath.Join(stateDir, stateFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	p := New(Options{API: srv.URL, Repo: "owner/repo", Exe: exe})
	if err := p.Init(plugin.InitContext{StateDir: stateDir}, map[string]any{"auto_check": false}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	st, err := p.ActionState(actionUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != plugin.ActionDone || !strings.Contains(st.Message, version.Version) {
		t.Fatalf("重启后没把上次的更新定稿: %+v", st)
	}
	// 认领过就该销账，否则每次启动都再宣布一遍
	if loadState(stateDir).Pending != nil {
		t.Fatal("pending 没有被清掉")
	}
}

// 换过文件却仍跑着旧版：说清楚现状，并且不要一直挂着那条 pending。
func TestReconcileVersionMismatch(t *testing.T) {
	srv := fakeGitHub(t, newTag)
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, updater.HostBinaryName())
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state{Pending: &pendingUpdate{From: version.Version, To: newTag, At: time.Now()}})
	if err := os.WriteFile(filepath.Join(stateDir, stateFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	p := New(Options{API: srv.URL, Repo: "owner/repo", Exe: exe})
	if err := p.Init(plugin.InitContext{StateDir: stateDir}, map[string]any{"auto_check": false}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	st, _ := p.ActionState(actionUpdate)
	if st.Status != plugin.ActionError || !strings.Contains(st.Message, newTag) {
		t.Fatalf("没有报出「换了但没生效」: %+v", st)
	}
	if loadState(stateDir).Pending != nil {
		t.Fatal("pending 没有被清掉，下次启动会再宣布一遍")
	}
}

// 进行中不接受第二次触发：半路重来只会留下一份没人认领的下载。
func TestRejectsConcurrentTrigger(t *testing.T) {
	// 让接口一直挂着，制造一个「进行中」的状态
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	p, _, _ := newPlugin(t, srv, nil)
	if err := p.StartAction(context.Background(), actionUpdate); err != nil {
		t.Fatal(err)
	}
	if err := p.StartAction(context.Background(), actionUpdate); err == nil {
		t.Fatal("进行中的第二次触发该被拒绝")
	}
}

// 下次检查的时刻按「上次检查 + 周期」推算，失败过就再压后一个重试间隔，
// 且一律不早于启动宽限期。
func TestNextCheck(t *testing.T) {
	base := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	p := New(Options{})
	p.interval = 24 * time.Hour

	// 从没查过：等到宽限期
	earliest := base.Add(startupGrace)
	if got := p.nextCheck(earliest); !got.Equal(earliest) {
		t.Fatalf("没查过时该等宽限期: %v", got)
	}

	// 查过了：上次 + 周期
	p.st.LastCheck = base
	want := base.Add(24 * time.Hour)
	if got := p.nextCheck(earliest); !got.Equal(want) {
		t.Fatalf("下次检查时刻不符: %v，期望 %v", got, want)
	}

	// 刚失败过一次：不能立刻重试，否则断网时就是一刻不停地重连
	p.st.LastCheck = time.Time{}
	p.lastTry = base
	if got := p.nextCheck(earliest); !got.After(base.Add(retryAfter - time.Second)) {
		t.Fatalf("失败后没有退避: %v", got)
	}
}

// 这个插件一个字都不该进模型上下文。
func TestInjectsNothing(t *testing.T) {
	p := New(Options{})
	if p.SystemPrompt() != "" {
		t.Fatalf("不该注入提示词: %q", p.SystemPrompt())
	}
	if len(p.Tools()) != 0 {
		t.Fatal("不该提供工具")
	}
}

// 没有持久化目录时拒绝启用，而不是退化到写进程当前目录。
func TestInitRequiresStateDir(t *testing.T) {
	p := New(Options{})
	if err := p.Init(plugin.InitContext{}, nil); err == nil {
		t.Fatal("没有 StateDir 却启用了")
	}
}
