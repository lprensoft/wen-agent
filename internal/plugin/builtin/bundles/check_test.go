package bundles

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/plugin"
)

func TestBackgroundChecksOnlySignedManifest(t *testing.T) {
	base := t.TempDir()
	p := newAt(t, base)
	if _, err := p.store.Install(writeBundle(t, filepath.Join(base, "src"))); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schema_version":1,"name":"demo-pack","version":"1.1.0","archive_url":"https://example.com/pkg.zip","sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest))
	var manifestCalls, archiveCalls atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update.json":
			manifestCalls.Add(1)
			_, _ = w.Write(manifest)
		case "/update.json.sig":
			_, _ = w.Write([]byte(sig))
		case "/pkg.zip":
			archiveCalls.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = ts.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	if err := p.store.Track("demo-pack", ts.URL+"/update.json", base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	next, ok := p.NextBackgroundAt(started)
	if !ok || !next.Equal(started.Add(checkStartupGrace)) {
		t.Fatalf("首次检查没有启动宽限：%v, %v", next, ok)
	}
	checkAt := started.Add(3 * time.Minute)
	if err := p.RunBackground(context.Background(), checkAt); err != nil {
		t.Fatalf("后台检查失败：%v", err)
	}
	src, _ := p.store.Source("demo-pack")
	if src.Candidate == nil || src.Candidate.Version != "1.1.0" || !src.LastCheck.Equal(checkAt) {
		t.Fatalf("后台候选不对：%+v", src)
	}
	if archiveCalls.Load() != 0 {
		t.Fatal("后台检查下载了 ZIP")
	}
	next, ok = p.NextBackgroundAt(checkAt)
	if !ok || !next.Equal(checkAt.Add(24*time.Hour)) {
		t.Fatalf("成功后的排期不对：%v, %v", next, ok)
	}
	calls := manifestCalls.Load()
	if err := p.RunBackground(context.Background(), checkAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if manifestCalls.Load() != calls {
		t.Fatal("未到周期又检查了一次")
	}

	// 下一轮签名变坏：记录失败，且至少一小时后才允许重试。
	sig = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	failedAt := checkAt.Add(25 * time.Hour)
	if err := p.RunBackground(context.Background(), failedAt); err == nil {
		t.Fatal("坏签名没有报错")
	}
	next, ok = p.NextBackgroundAt(failedAt)
	if !ok || next.Before(failedAt.Add(checkRetryAfter)) {
		t.Fatalf("失败后的退避不对：%v, %v", next, ok)
	}
}

func TestTrackAndUntrackActions(t *testing.T) {
	base := t.TempDir()
	p := newAt(t, base)
	if _, err := p.store.Install(writeBundle(t, filepath.Join(base, "src"))); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	values := map[string]any{
		"update_name": "demo-pack",
		"update_url":  "https://example.com/update.json",
		"update_key":  base64.StdEncoding.EncodeToString(pub),
	}
	if err := p.StartAction(plugin.WithActionValues(context.Background(), values), actionTrack); err != nil {
		t.Fatal(err)
	}
	if st := waitAction(t, p, actionTrack); st.Status != plugin.ActionDone {
		t.Fatalf("绑定失败：%+v", st)
	}
	if src, _ := p.store.Source("demo-pack"); src == nil {
		t.Fatal("操作没有写入来源")
	}
	if err := p.StartAction(plugin.WithActionValues(context.Background(), values), actionUntrack); err != nil {
		t.Fatal(err)
	}
	if st := waitAction(t, p, actionUntrack); st.Status != plugin.ActionDone {
		t.Fatalf("解绑失败：%+v", st)
	}
	if src, _ := p.store.Source("demo-pack"); src != nil {
		t.Fatal("解绑后来源还在")
	}
}

func TestBackgroundIgnoresNullSourceEntries(t *testing.T) {
	base := t.TempDir()
	p := newAt(t, base)
	if err := os.MkdirAll(filepath.Join(base, "bundles", ".updates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "bundles", ".updates", "sources.json"), []byte(`{"demo-pack":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.NextBackgroundAt(time.Now()); ok {
		t.Fatal("null 来源条目不该形成排期")
	}
	if err := p.RunBackground(context.Background(), time.Now()); err != nil {
		t.Fatalf("null 来源条目不该让后台失败：%v", err)
	}
	_ = p.StatusLines("") // 不因空条目 panic
}

func TestManualTimeAndDisabledCheckHaveNoLoop(t *testing.T) {
	p := New("", t.TempDir())
	if err := p.Init(plugin.InitContext{ManualTime: true}, map[string]any{"auto_check": false}); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if p.cancel != nil {
		t.Fatal("ManualTime 下启动了真实循环")
	}
	if _, ok := p.NextBackgroundAt(time.Now()); ok {
		t.Fatal("关闭自动检查后仍有排期")
	}
}
