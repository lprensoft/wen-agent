package agentplugin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUpdateLockBlocksAndIgnoresAbandonedFile(t *testing.T) {
	base := t.TempDir()
	first := NewStore("", base)
	second := NewStore("", base)
	release, err := first.acquireUpdateLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := second.acquireUpdateLock(ctx); err == nil {
		release()
		t.Fatal("另一个进程实例没有等待文件锁")
	}
	release()
	releaseSecond, err := second.acquireUpdateLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	releaseSecond()

	// 锁文件内容不是锁本身：崩溃留下的文件不应阻塞新的操作系统锁。
	lockPath := filepath.Join(first.updatesDir(), updateLockFile)
	if err := os.WriteFile(lockPath, []byte("crashed"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err = first.acquireUpdateLock(context.Background())
	if err != nil {
		t.Fatalf("遗留锁文件不该阻塞新锁：%v", err)
	}
	release()
}

func TestConcurrentTrackAcrossStoresDoesNotLoseSources(t *testing.T) {
	base := t.TempDir()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key := base64.StdEncoding.EncodeToString(pub)
	const count = 16

	// 每个 Store 都有自己的 opMu；只有文件锁能覆盖这种跨实例/跨进程写法。
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("pkg-%02d", i)
		manifest := fmt.Sprintf(`{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":%q,"version":"1.0.0"}`, name)
		writeBundle(t, filepath.Join(base, "bundles", name), manifest, nil, "")
	}

	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("pkg-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := NewStore("", base)
			if err := store.Track(name, "https://example.com/"+name+"/update.json", key); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发绑定失败：%v", err)
	}
	sources, err := NewStore("", base).Sources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != count {
		t.Fatalf("并发绑定后只剩 %d 个来源，想要 %d", len(sources), count)
	}
}

func TestSourcesRejectsNullDocument(t *testing.T) {
	base := t.TempDir()
	store := NewStore("", base)
	if err := os.MkdirAll(store.updatesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.sourcesPath(), []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	writeBundle(t, store.Path("demo"), `{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"demo","version":"1.0.0"}`, nil, "")
	err := store.Track("demo", "https://example.com/update.json", base64.StdEncoding.EncodeToString(pub))
	if err == nil || !strings.Contains(err.Error(), "JSON 对象") {
		t.Fatalf("顶层 null 该报告来源表不是对象，实际：%v", err)
	}
}

func TestSourcesSkipsNullEntries(t *testing.T) {
	base := t.TempDir()
	store := NewStore("", base)
	if err := os.MkdirAll(store.updatesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.sourcesPath(), []byte(`{"demo":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, err := store.Sources()
	if err != nil || len(sources) != 0 {
		t.Fatalf("null 条目没有被安全忽略：%+v, %v", sources, err)
	}
	if _, err := store.CheckUpdate(context.Background(), "demo"); err == nil {
		t.Fatal("null 条目不该被当成已绑定来源")
	}
}
