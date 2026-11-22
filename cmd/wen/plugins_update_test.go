package main

import (
	"archive/zip"
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
	"testing"

	"wen/internal/agentplugin"
)

func TestUninstallRejectsTraversalWithoutDeletingCache(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "bundles", "victim.json")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := agentplugin.NewStore("", base)
	if err := uninstallBundle(store, "../victim", base); err == nil {
		t.Fatal("含路径分隔符的包名不该通过")
	}
	if body, err := os.ReadFile(victim); err != nil || string(body) != "keep" {
		t.Fatalf("校验失败误删了缓存目录之外的文件：%q, %v", body, err)
	}
}

func TestPluginUpdateCommands(t *testing.T) {
	base := t.TempDir()
	store := agentplugin.NewStore("", base)
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(filepath.Join(src, "skills", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","version":"1.0.0"}`
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "skills", "a", "SKILL.md"), []byte("---\nname: a\ndescription: d\n---\nold"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Install(src); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(base, "new.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	entry, _ := zw.Create("plugin.json")
	_, _ = entry.Write([]byte(`{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","version":"1.1.0"}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	archive, _ := os.ReadFile(zipPath)
	sum := sha256.Sum256(archive)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var updateManifest []byte
	var signature string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update.json":
			_, _ = w.Write(updateManifest)
		case "/update.json.sig":
			_, _ = w.Write([]byte(signature))
		case "/pkg.zip":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	updateManifest = []byte(fmt.Sprintf(`{"schema_version":1,"name":"demo","version":"1.1.0","archive_url":%q,"sha256":%q}`, ts.URL+"/pkg.zip", hex.EncodeToString(sum[:])))
	signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, updateManifest))
	oldTransport := http.DefaultTransport
	http.DefaultTransport = ts.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	oldLang := uiLang
	uiLang = ""
	defer func() { uiLang = oldLang }()

	if err := trackBundle(store, "demo", ts.URL+"/update.json", base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatalf("track：%v", err)
	}
	if err := checkBundles(store, "demo"); err != nil {
		t.Fatalf("check：%v", err)
	}
	if err := updateBundle(store, "demo", false); err != nil {
		t.Fatalf("update：%v", err)
	}
	if pending, _ := store.Pending("demo"); pending == nil || pending.Version != "1.1.0" {
		t.Fatalf("update 没有暂存：%+v", pending)
	}
	if err := updateBundle(store, "demo", true); err != nil {
		t.Fatalf("cancel：%v", err)
	}
	if pending, _ := store.Pending("demo"); pending != nil {
		t.Fatal("cancel 后 pending 还在")
	}
	if err := checkBundles(store, ""); err != nil {
		t.Fatalf("check all：%v", err)
	}
	if err := untrackBundle(store, "demo"); err != nil {
		t.Fatalf("untrack：%v", err)
	}
}
