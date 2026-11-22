package main

import (
	"testing"

	"wen/internal/plugin/builtin/bundles"
)

func TestEvalSuppressesBundleUpdateMaintenance(t *testing.T) {
	if !evalSuppressed(bundles.New("", t.TempDir())) {
		t.Fatal("评测不该驱动真实插件包更新检查")
	}
}
