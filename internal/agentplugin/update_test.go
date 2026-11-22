package agentplugin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

func installUpdatableBundle(t *testing.T, store *Store, version string) {
	t.Helper()
	manifest := fmt.Sprintf(`{
	  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
	  "name": "pdf-tools", "version": %q
	}`, version)
	src := writeBundle(t, filepath.Join(t.TempDir(), "src"), manifest,
		map[string]string{"a": "---\nname: a\ndescription: d\n---\nold"}, "")
	if _, err := store.Install(src); err != nil {
		t.Fatalf("安装测试包失败：%v", err)
	}
}

func signedManifest(t *testing.T, key ed25519.PrivateKey, version string) ([]byte, string) {
	t.Helper()
	manifest := []byte(fmt.Sprintf(`{"schema_version":1,"name":"pdf-tools","version":%q,"archive_url":"https://example.com/pkg.zip","sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`, version))
	sig := ed25519.Sign(key, manifest)
	return manifest, base64.StdEncoding.EncodeToString(sig)
}

func TestTrackAndCheckSignedUpdate(t *testing.T) {
	store := NewStore("", t.TempDir())
	installUpdatableBundle(t, store, "1.2.0")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest, sig := signedManifest(t, priv, "1.3.0")
	var archiveCalls atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update.json":
			_, _ = w.Write(manifest)
		case "/update.json.sig":
			_, _ = w.Write([]byte(sig))
		case "/pkg.zip":
			archiveCalls.Add(1)
			t.Error("检查更新不该下载 ZIP")
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	store.client = ts.Client()
	manifestURL := ts.URL + "/update.json"
	publicKey := base64.StdEncoding.EncodeToString(pub)
	if err := store.Track("pdf-tools", manifestURL, publicKey); err != nil {
		t.Fatalf("绑定来源失败：%v", err)
	}

	candidate, err := store.CheckUpdate(context.Background(), "pdf-tools")
	if err != nil {
		t.Fatalf("检查更新失败：%v", err)
	}
	if candidate == nil || candidate.Version != "1.3.0" || candidate.Token == "" {
		t.Fatalf("候选不对：%+v", candidate)
	}
	if archiveCalls.Load() != 0 {
		t.Fatal("检查阶段请求了 ZIP")
	}
	src, err := NewStore("", filepath.Dir(store.Dir())).Source("pdf-tools")
	if err != nil {
		t.Fatal(err)
	}
	if src == nil || src.Candidate == nil || src.LastCheck.IsZero() || src.LastError != "" {
		t.Fatalf("来源状态没有持久化：%+v", src)
	}
	fingerprint, err := PublicKeyFingerprint("", publicKey)
	if err != nil || len(fingerprint) != 16 {
		t.Fatalf("公钥指纹不对：%q, %v", fingerprint, err)
	}

	// 同版与更旧版本都不是更新，也不会留下候选。
	for _, version := range []string{"1.2.0", "1.1.9"} {
		manifest, sig = signedManifest(t, priv, version)
		candidate, err = store.CheckUpdate(context.Background(), "pdf-tools")
		if err != nil || candidate != nil {
			t.Fatalf("版本 %s 不该产生更新：%+v, %v", version, candidate, err)
		}
	}
}

func TestCheckUpdateRejectsBadTrustAndVersions(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manifest, sig := signedManifest(t, priv, "1.3.0")
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/update.json.sig" {
			_, _ = w.Write([]byte(sig))
			return
		}
		_, _ = w.Write(manifest)
	}))
	defer ts.Close()

	store := NewStore("", t.TempDir())
	installUpdatableBundle(t, store, "1.2.0")
	store.client = ts.Client()
	if err := store.Track("pdf-tools", ts.URL+"/update.json", base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}

	t.Run("错误签名", func(t *testing.T) {
		old := sig
		sig = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		defer func() { sig = old }()
		if _, err := store.CheckUpdate(context.Background(), "pdf-tools"); err == nil {
			t.Fatal("错误签名不该通过")
		}
		src, _ := store.Source("pdf-tools")
		if src.LastError == "" || !src.LastCheck.IsZero() {
			t.Fatalf("失败不该记成成功检查：%+v", src)
		}
	})

	t.Run("无法比较的目标版本", func(t *testing.T) {
		oldM, oldS := manifest, sig
		manifest, sig = signedManifest(t, priv, "next")
		defer func() { manifest, sig = oldM, oldS }()
		if _, err := store.CheckUpdate(context.Background(), "pdf-tools"); err == nil {
			t.Fatal("无法比较的版本不该通过")
		}
	})

	t.Run("包名不一致", func(t *testing.T) {
		oldM, oldS := manifest, sig
		manifest = []byte(`{"schema_version":1,"name":"other","version":"1.3.0","archive_url":"https://example.com/pkg.zip","sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`)
		sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest))
		defer func() { manifest, sig = oldM, oldS }()
		if _, err := store.CheckUpdate(context.Background(), "pdf-tools"); err == nil {
			t.Fatal("包名不一致不该通过")
		}
	})
}

func TestTrackValidationAndRebind(t *testing.T) {
	store := NewStore("", t.TempDir())
	installUpdatableBundle(t, store, "1.2.0")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)
	if err := store.Track("pdf-tools", "http://example.com/update.json", key); err == nil {
		t.Fatal("HTTP 来源不该通过")
	}
	if err := store.Track("pdf-tools", " https://example.com/update.json", key); err == nil {
		t.Fatal("带首尾空格的 URL 不该通过")
	}
	if err := store.Track("pdf-tools", "https://example.com/update.json", "bad"); err == nil {
		t.Fatal("坏公钥不该通过")
	}
	if err := store.Track("pdf-tools", "https://one.example/update.json", key); err != nil {
		t.Fatal(err)
	}
	if err := store.Track("pdf-tools", "https://two.example/update.json", key); err == nil {
		t.Fatal("换来源必须先解除绑定")
	}
	if err := store.Untrack("pdf-tools"); err != nil {
		t.Fatal(err)
	}
	if src, _ := store.Source("pdf-tools"); src != nil {
		t.Fatal("解除后来源还在")
	}
}

func TestTrackRequiresComparableCurrentVersion(t *testing.T) {
	store := NewStore("", t.TempDir())
	installUpdatableBundle(t, store, "next")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Track("pdf-tools", "https://example.com/update.json", base64.StdEncoding.EncodeToString(pub)); err == nil {
		t.Fatal("无法比较的当前版本不该绑定")
	}
}

func TestUpdateRejectsHTTPSDowngradeRedirect(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("must not be fetched as an update"))
	}))
	defer plain.Close()
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer tlsServer.Close()

	store := NewStore("", t.TempDir())
	installUpdatableBundle(t, store, "1.2.0")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Track("pdf-tools", tlsServer.URL+"/update.json", base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	client := newUpdateClient("")
	client.Transport = tlsServer.Client().Transport
	store.client = client
	if _, err := store.CheckUpdate(context.Background(), "pdf-tools"); err == nil {
		t.Fatal("HTTPS 跳成 HTTP 不该通过")
	}
}

func TestTrackRejectsMismatchedManifestName(t *testing.T) {
	store := NewStore("", t.TempDir())
	writeBundle(t, store.Path("expected"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"other","version":"1.0.0"}`, nil, "")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Track("expected", "https://example.com/update.json", base64.StdEncoding.EncodeToString(pub)); err == nil {
		t.Fatal("目录名与清单包名不一致不该绑定")
	}
}

func TestUpdateSourceFilesArePrivate(t *testing.T) {
	store := NewStore("", t.TempDir())
	installUpdatableBundle(t, store, "1.2.0")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Track("pdf-tools", "https://example.com/update.json", base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.sourcesPath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("来源文件权限为 %o，想要 600", info.Mode().Perm())
	}
}
