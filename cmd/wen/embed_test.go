package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"wen/internal/config"
	"wen/internal/modelcfg"
	"wen/internal/plugin"
)

func TestEmbedFuncsFollowConfigLive(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":[[3,4]]}`))
	}))
	defer remote.Close()

	store, err := modelcfg.NewStore("", filepath.Join(t.TempDir(), "models.json"), &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	embed, model := embedFuncs("", func() *modelcfg.Store { return store })

	// 没配过：关着，且报的是「未启用」这个常态错误而不是别的
	if model() != "" {
		t.Fatal("没配过嵌入，身份必须为空")
	}
	if _, err := embed(context.Background(), []string{"a"}); !errors.Is(err, plugin.ErrEmbedUnavailable) {
		t.Fatalf("未启用时应返回 ErrEmbedUnavailable，得到 %v", err)
	}

	// 保存即生效：不需要重建闭包，也不需要重新 Init 任何插件
	if _, err := store.SetEmbedding("", modelcfg.Embedding{
		Enabled: true, Type: "ollama", BaseURL: remote.URL, Model: "m",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := embed(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != model() || res.Model == "" {
		t.Errorf("结果的身份 %q 应与当前身份 %q 一致", res.Model, model())
	}
	if len(res.Vectors) != 1 || res.Vectors[0][0] < 0.59 || res.Vectors[0][0] > 0.61 {
		t.Errorf("向量应已归一化：%v", res.Vectors)
	}

	// 关掉：立即回到未启用
	if _, err := store.SetEmbedding("", modelcfg.Embedding{Type: "ollama", BaseURL: remote.URL, Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if model() != "" {
		t.Error("关掉之后身份应为空")
	}

	// 模型还没就绪（nil）同样当作未启用，不能 panic
	_, nilModel := embedFuncs("", func() *modelcfg.Store { return nil })
	if nilModel() != "" {
		t.Error("没有模型配置时应当当作未启用")
	}
}
