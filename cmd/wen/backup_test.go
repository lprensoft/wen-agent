package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"wen/internal/config"
	"wen/internal/runlock"
)

func writeBackupTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readBackupTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBackupCommandsRegistered(t *testing.T) {
	for _, name := range []string{"export", "import"} {
		if lookup(name) == nil {
			t.Errorf("command %q is not registered", name)
		}
	}
}

func TestPathWithin(t *testing.T) {
	base := t.TempDir()
	if rel, ok := pathWithin(base, filepath.Join(base, "sessions")); !ok || rel != "sessions" {
		t.Fatalf("inside: %q %v", rel, ok)
	}
	if _, ok := pathWithin(base, filepath.Dir(base)); ok {
		t.Fatal("parent should be outside")
	}
	if rel, ok := pathWithin(base, base); !ok || rel != "." {
		t.Fatalf("base itself: %q %v", rel, ok)
	}
}

func TestProjectMemoryDirs(t *testing.T) {
	base := t.TempDir()
	work := t.TempDir()
	writeBackupTestFile(t, filepath.Join(work, ".wen", "memories", "a.md"), "x")
	writeBackupTestFile(t, filepath.Join(work, ".wen", "memories-inner", "b.md"), "y")
	writeBackupTestFile(t, filepath.Join(base, "plugins.state.json"), `{"memory":{"enabled":true,"config":{"scope":"project"}}}`)

	cfg := &config.Config{BaseDir: base, Agent: config.AgentConfig{Workdir: work}}
	want := []string{
		filepath.Join(work, ".wen", "memories"),
		filepath.Join(work, ".wen", "memories-inner"),
	}
	if got := projectMemoryDirs(cfg); !slices.Equal(got, want) {
		t.Fatalf("projectMemoryDirs = %q, want %q", got, want)
	}

	writeBackupTestFile(t, filepath.Join(base, "plugins.state.json"), `{"memory":{"enabled":true,"config":{"scope":"global"}}}`)
	if got := projectMemoryDirs(cfg); len(got) != 0 {
		t.Fatalf("global scope should not report project directories: %q", got)
	}
	writeBackupTestFile(t, filepath.Join(base, "plugins.state.json"), "not json")
	if got := projectMemoryDirs(cfg); len(got) != 0 {
		t.Fatalf("invalid state should be ignored: %q", got)
	}
}

func TestServiceRunning(t *testing.T) {
	dir := t.TempDir()
	if _, ok := serviceRunning(dir); ok {
		t.Fatal("no lock file should mean not running")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	release, err := runlock.Acquire(dir, runlock.Info{Addr: srv.Listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if addr, ok := serviceRunning(dir); !ok || addr != srv.Listener.Addr().String() {
		t.Fatalf("serviceRunning = %q %v", addr, ok)
	}
	release()
	srv.Close()
	if _, ok := serviceRunning(dir); ok {
		t.Fatal("closed service should not be reported as running")
	}
}

func TestExportImportCommands(t *testing.T) {
	base := t.TempDir()
	writeBackupTestFile(t, filepath.Join(base, "config.yaml"), "lang: zh\n")
	writeBackupTestFile(t, filepath.Join(base, "models.json"), `{"version":1,"providers":[],"current":{}}`)
	writeBackupTestFile(t, filepath.Join(base, "sessions", "s1.jsonl"), "{\"type\":\"meta\"}\n")

	cfgPath := filepath.Join(base, "config.yaml")
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := runExport([]string{"-c", cfgPath, "-o", archive}); err != nil {
		t.Fatalf("runExport: %v", err)
	}

	target := t.TempDir()
	targetCfg := filepath.Join(target, "config.yaml")
	if err := runImport([]string{archive, "-c", targetCfg}); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if got := readBackupTestFile(t, filepath.Join(target, "sessions", "s1.jsonl")); got != "{\"type\":\"meta\"}\n" {
		t.Fatalf("restored session = %q", got)
	}
	if got := readBackupTestFile(t, filepath.Join(target, "config.yaml")); got != "lang: zh\n" {
		t.Fatalf("restored config = %q", got)
	}
}

func TestExportIncludesProjectMemoryDomains(t *testing.T) {
	base := t.TempDir()
	writeBackupTestFile(t, filepath.Join(base, "config.yaml"), fmt.Sprintf("lang: zh\nagent:\n  workdir: %q\n", base))
	writeBackupTestFile(t, filepath.Join(base, "plugins.state.json"), `{"memory":{"enabled":true,"config":{"scope":"project"}}}`)
	writeBackupTestFile(t, filepath.Join(base, ".wen", "memories", "a.md"), "shared\n")
	writeBackupTestFile(t, filepath.Join(base, ".wen", "memories-inner", "b.md"), "inner\n")

	archive := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := runExport([]string{"-c", filepath.Join(base, "config.yaml"), "-o", archive}); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	target := t.TempDir()
	if err := runImport([]string{archive, "-c", filepath.Join(target, "config.yaml")}); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	for _, name := range []string{
		filepath.Join(".wen", "memories", "a.md"),
		filepath.Join(".wen", "memories-inner", "b.md"),
	} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("project memory domain file %s was not restored: %v", name, err)
		}
	}
}

func TestExportRefusesWhileRunning(t *testing.T) {
	base := t.TempDir()
	writeBackupTestFile(t, filepath.Join(base, "config.yaml"), "lang: zh\n")
	writeBackupTestFile(t, filepath.Join(base, "models.json"), `{"version":1,"providers":[],"current":{}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	release, err := runlock.Acquire(base, runlock.Info{Addr: srv.Listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := runExport([]string{"-c", filepath.Join(base, "config.yaml"), "-o", out}); err == nil {
		t.Fatal("export should refuse while the service is running")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("no archive should be written on refusal: %v", err)
	}
}
