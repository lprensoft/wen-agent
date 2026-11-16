package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"wen/internal/config"
	"wen/internal/modelcfg"
)

func newEmbeddingServer(t *testing.T) *Server {
	t.Helper()
	store, err := modelcfg.NewStore("", filepath.Join(t.TempDir(), "models.json"), &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{models: store}
}

func callEmbedding(t *testing.T, h http.HandlerFunc, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestEmbeddingViewNeverExposesKey(t *testing.T) {
	s := newEmbeddingServer(t)
	code, out := callEmbedding(t, s.saveEmbedding, map[string]any{
		"enabled": true, "type": "openai_compat", "base_url": "https://api.example.com/v1",
		"api_key": "sk-secret-value-1234", "model": "m",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "sk-secret-value-1234") || out["has_api_key"] != true {
		t.Fatalf("响应不能带明文密钥：%s", raw)
	}
	if out["api_key_masked"] == "" {
		t.Error("应给出密钥掩码")
	}
}

func TestSaveEmbeddingKeepsKeyWhenLeftBlank(t *testing.T) {
	s := newEmbeddingServer(t)
	body := map[string]any{"enabled": true, "type": "openai_compat", "base_url": "https://api.example.com/v1",
		"api_key": "sk-1", "model": "m"}
	callEmbedding(t, s.saveEmbedding, body)
	body["api_key"], body["model"] = "", "m2"
	if code, out := callEmbedding(t, s.saveEmbedding, body); code != http.StatusOK {
		t.Fatalf("留空密钥应沿用：%d %v", code, out)
	}
	if got := s.models.Embedding(); got.APIKey != "sk-1" || got.Model != "m2" {
		t.Errorf("保存结果 = %+v", got)
	}
}

func TestSaveEmbeddingRejectsInvalid(t *testing.T) {
	s := newEmbeddingServer(t)
	code, out := callEmbedding(t, s.saveEmbedding, map[string]any{"enabled": true, "type": "openai_compat"})
	if code != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("开启却没填地址与模型应被拒绝：%d %v", code, out)
	}
	if s.models.Embedding().Enabled {
		t.Error("被拒绝的保存不该生效")
	}
}

func TestTestEmbeddingReportsDimensionsAndErrors(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
	}))
	defer remote.Close()
	s := newEmbeddingServer(t)

	// 没开启也能测：用户正是想在打开之前先试一下
	code, out := callEmbedding(t, s.testEmbedding, map[string]any{
		"type": "openai_compat", "base_url": remote.URL, "api_key": "good", "model": "m"})
	if code != http.StatusOK || out["dimensions"] != float64(3) {
		t.Fatalf("code=%d out=%v", code, out)
	}

	code, out = callEmbedding(t, s.testEmbedding, map[string]any{
		"type": "openai_compat", "base_url": remote.URL, "api_key": "bad", "model": "m"})
	if code != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("密钥错误应报连接失败：%d %v", code, out)
	}
	if s.models.Embedding().Enabled {
		t.Error("测试不该改动已保存的配置")
	}
}
