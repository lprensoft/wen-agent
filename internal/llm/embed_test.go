package llm

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestEmbedder(t *testing.T, typ, url, key string, dims int) Embedder {
	t.Helper()
	e, err := NewEmbedder(EmbedConfig{Type: typ, BaseURL: url, APIKey: key, Model: "m", Dimensions: dims})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEmbedOpenAIShapeNormalizesAndKeepsOrder(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// 故意乱序返回，靠 index 还原
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,3]},{"index":0,"embedding":[3,4]}]}`))
	}))
	defer srv.Close()

	e := newTestEmbedder(t, EmbedTypeOpenAI, srv.URL+"/v1/", "sk-test", 2)
	vecs, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/embeddings" || gotAuth != "Bearer sk-test" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["dimensions"] != float64(2) || gotBody["model"] != "m" {
		t.Fatalf("body = %v", gotBody)
	}
	if math.Abs(float64(vecs[0][0])-0.6) > 1e-6 || math.Abs(float64(vecs[0][1])-0.8) > 1e-6 {
		t.Fatalf("第一个向量应是 [3 4] 归一化后的 [0.6 0.8]，得到 %v", vecs[0])
	}
	if vecs[1][0] != 0 || math.Abs(float64(vecs[1][1])-1) > 1e-6 {
		t.Fatalf("第二个向量 = %v", vecs[1])
	}
}

func TestEmbedOllamaShapeAndNoAuthHeader(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"embeddings":[[1,0]]}`))
	}))
	defer srv.Close()

	e := newTestEmbedder(t, EmbedTypeOllama, srv.URL, "", 0)
	if _, err := e.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/embed" || gotAuth != "" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if _, has := gotBody["dimensions"]; has {
		t.Fatalf("维度为 0 时不该发 dimensions：%v", gotBody)
	}
}

func TestEmbedSplitsIntoBatches(t *testing.T) {
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		sizes = append(sizes, len(req.Input))
		var data []map[string]any
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	texts := make([]string, embedBatchSize*2+5)
	for i := range texts {
		texts[i] = "x"
	}
	vecs, err := newTestEmbedder(t, EmbedTypeOpenAI, srv.URL, "", 0).Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != len(texts) || len(sizes) != 3 || sizes[2] != 5 {
		t.Fatalf("vecs=%d sizes=%v", len(vecs), sizes)
	}
}

func TestEmbedRejectsMalformedResponses(t *testing.T) {
	cases := map[string]string{
		"条数对不上": `{"data":[{"index":0,"embedding":[1,0]}]}`,
		"空向量":   `{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[1]}]}`,
		"全零向量":  `{"data":[{"index":0,"embedding":[0,0]},{"index":1,"embedding":[1,0]}]}`,
		"维度不一致": `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[1,0,0]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			if _, err := newTestEmbedder(t, EmbedTypeOpenAI, srv.URL, "", 0).
				Embed(context.Background(), []string{"a", "b"}); err == nil {
				t.Fatal("应当报错")
			}
		})
	}
}

func TestEmbedHTTPErrorIsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := newTestEmbedder(t, EmbedTypeOpenAI, srv.URL, "k", 0).Embed(context.Background(), []string{"a"})
	if !IsConfigError(err) {
		t.Fatalf("401 应归为配置错误：%v", err)
	}
}

func TestNewEmbedderValidates(t *testing.T) {
	for name, cfg := range map[string]EmbedConfig{
		"没填模型": {Type: EmbedTypeOpenAI, BaseURL: "http://x"},
		"没填地址": {Type: EmbedTypeOpenAI, Model: "m"},
		"未知类型": {Type: "nope", BaseURL: "http://x", Model: "m"},
		"负维度":  {Type: EmbedTypeOpenAI, BaseURL: "http://x", Model: "m", Dimensions: -1},
	} {
		if _, err := NewEmbedder(cfg); err == nil {
			t.Errorf("%s：应当报错", name)
		}
	}
}

func TestClipRunesDoesNotSplitCharacters(t *testing.T) {
	got := clipRunes(strings.Repeat("记", 10), 4)
	if got != "记记记记" {
		t.Fatalf("got %q", got)
	}
}

func TestEmbedIdentityIgnoresAddressAndKey(t *testing.T) {
	a := EmbedConfig{Type: EmbedTypeOpenAI, Model: "m", Dimensions: 256, BaseURL: "http://a", APIKey: "1"}
	b := EmbedConfig{Type: EmbedTypeOpenAI, Model: "m", Dimensions: 256, BaseURL: "http://b", APIKey: "2"}
	if a.Identity() != b.Identity() {
		t.Fatal("换地址或密钥不该改变向量身份")
	}
	b.Dimensions = 512
	if a.Identity() == b.Identity() {
		t.Fatal("维度不同的向量不能混用")
	}
}
