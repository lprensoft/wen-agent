package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"wen/internal/i18n"
)

// 嵌入（文本向量化）与聊天是两件事：Anthropic 没有嵌入模型，聊天走它的人仍可能
// 用 OpenAI、Voyage 或本机的 Ollama 做嵌入，所以它有自己的一套类型与配置，
// 不复用 Provider。

// 嵌入服务的接口形状。
const (
	// EmbedTypeOpenAI 是 POST {base}/embeddings，OpenAI、Voyage、硅基流动、智谱等都是这个形状。
	EmbedTypeOpenAI = "openai_compat"
	// EmbedTypeOllama 是 Ollama 原生的 POST {base}/api/embed，base 不带 /v1。
	EmbedTypeOllama = "ollama"
)

// KnownEmbedTypes 是支持的嵌入接口形状（配置校验与界面下拉共用，避免两处漂移）。
var KnownEmbedTypes = []string{EmbedTypeOpenAI, EmbedTypeOllama}

// IsKnownEmbedType 判断接口形状是否受支持。
func IsKnownEmbedType(t string) bool { return slices.Contains(KnownEmbedTypes, t) }

// EmbedTypeLabel 返回接口形状的显示名。它进设置页的下拉框，按请求语言取。
func EmbedTypeLabel(lang, typ string) string {
	switch typ {
	case EmbedTypeOpenAI:
		return i18n.T(lang, "llm.embed.type.openai_compat", "OpenAI 兼容（/embeddings）")
	case EmbedTypeOllama:
		return i18n.T(lang, "llm.embed.type.ollama", "Ollama 原生（/api/embed）")
	}
	return typ
}

const (
	// embedBatchSize 是一次请求带的文本条数上限。OpenAI 允许到 2048，但各家的上限不一，
	// 已知最严的是通义千问 text-embedding-v4 的 10 条（预置里就有它），所以固定取 10：
	// 一个到处都放得过的值，代价只是首次建索引时多几个请求。连接测试只发一条，
	// 测不出批量上限，因此这个值必须保守，而不能靠测试连接兜底。
	embedBatchSize = 10
	// embedMaxRunes 是单条文本送出去的字数上限。OpenAI 的上限是 8192 token，
	// 中文按一字一到两个 token 算，2000 字留足余量，也避免一条超长记忆拖慢整批。
	embedMaxRunes = 2000
	// embedErrBodyBytes 是报错时保留的响应体长度。
	embedErrBodyBytes = 4096
)

// EmbedConfig 是创建 Embedder 所需的参数。
type EmbedConfig struct {
	Type    string
	BaseURL string
	APIKey  string
	Model   string
	// Dimensions 是请求的向量维度，0 表示用模型自己的默认维度。只有支持降维的模型才认
	//（OpenAI 的 3 代模型、Ollama 的部分模型）；不认的服务可能忽略，也可能报错。
	Dimensions int
	// Lang 是报错说话用的语言，取进程语言，理由同 Config.Lang。
	Lang string
}

// Identity 是这份配置产出的向量的身份：身份不同的向量不能放在一起比。
// 不含地址与密钥——同一个模型换个代理地址，向量仍然可比，不该为此重建整个索引。
func (c EmbedConfig) Identity() string {
	return fmt.Sprintf("%s|%s|%d", c.Type, c.Model, c.Dimensions)
}

// Embedder 把文本变成单位向量。
type Embedder interface {
	// Embed 按输入顺序返回向量，每个向量已归一化（点积即余弦相似度）。
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// NewEmbedder 按配置创建 Embedder。
func NewEmbedder(cfg EmbedConfig) (Embedder, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("%s", i18n.T(cfg.Lang, "llm.err.embed_no_model", "未填写嵌入模型"))
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("%s", i18n.T(cfg.Lang, "llm.err.embed_no_base_url", "未填写嵌入服务地址"))
	}
	if cfg.Dimensions < 0 {
		return nil, fmt.Errorf("%s", i18n.T(cfg.Lang, "llm.err.embed_bad_dims", "向量维度不能为负数"))
	}
	switch cfg.Type {
	case EmbedTypeOpenAI, EmbedTypeOllama:
		return &httpEmbedder{
			cfg:    cfg,
			base:   strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
			client: &http.Client{Timeout: 60 * time.Second},
		}, nil
	}
	return nil, fmt.Errorf(i18n.T(cfg.Lang, "llm.err.embed_bad_type",
		"不支持的嵌入接口 %q（可选：%s）"), cfg.Type, strings.Join(KnownEmbedTypes, " / "))
}

// ProbeEmbed 用一次真实的嵌入请求验证地址、密钥与模型是否可用，返回实际的向量维度。
// 设置页的「测试连接」与记忆插件的建索引共用同一个 Embedder，判据只有一份。
func ProbeEmbed(ctx context.Context, e Embedder) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vecs, err := e.Embed(ctx, []string{"ping"})
	if err != nil {
		return 0, err
	}
	return len(vecs[0]), nil
}

type httpEmbedder struct {
	cfg    EmbedConfig
	base   string
	client *http.Client
}

func (e *httpEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedBatchSize {
		end := min(start+embedBatchSize, len(texts))
		batch := make([]string, 0, end-start)
		for _, t := range texts[start:end] {
			batch = append(batch, clipRunes(t, embedMaxRunes))
		}
		vecs, err := e.embedBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// embedBatch 发一次请求并校验返回：条数要对得上、每个向量非空且维度一致。
// 服务端静默少返回几条时，调用方按位置对应就会把向量贴到错的文本上，必须当场报错。
func (e *httpEmbedder) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	var (
		path string
		body any
	)
	switch e.cfg.Type {
	case EmbedTypeOllama:
		path = "/api/embed"
		req := map[string]any{"model": e.cfg.Model, "input": batch}
		if e.cfg.Dimensions > 0 {
			req["dimensions"] = e.cfg.Dimensions
		}
		body = req
	default:
		path = "/embeddings"
		req := map[string]any{"model": e.cfg.Model, "input": batch}
		if e.cfg.Dimensions > 0 {
			req["dimensions"] = e.cfg.Dimensions
		}
		body = req
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 本机服务不做鉴权，密钥为空时不发这个头，理由同聊天请求。
	if e.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, embedErrBodyBytes))
		return nil, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(errBody))}
	}

	var vecs [][]float32
	switch e.cfg.Type {
	case EmbedTypeOllama:
		var parsed struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return nil, err
		}
		vecs = parsed.Embeddings
	default:
		var parsed struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return nil, err
		}
		// 规范保证按输入顺序，但 index 就在那里，按它排最稳妥。
		sort.SliceStable(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
		for _, d := range parsed.Data {
			vecs = append(vecs, d.Embedding)
		}
	}
	if len(vecs) != len(batch) {
		return nil, fmt.Errorf(i18n.T(e.cfg.Lang, "llm.err.embed_count",
			"嵌入服务返回了 %d 个向量，输入是 %d 条"), len(vecs), len(batch))
	}
	dim := 0
	for i, v := range vecs {
		if len(v) == 0 {
			return nil, fmt.Errorf("%s", i18n.T(e.cfg.Lang, "llm.err.embed_empty", "嵌入服务返回了空向量"))
		}
		if dim == 0 {
			dim = len(v)
		} else if len(v) != dim {
			return nil, fmt.Errorf("%s", i18n.T(e.cfg.Lang, "llm.err.embed_dim_mismatch", "嵌入服务返回的向量维度不一致"))
		}
		if !normalize(v) {
			return nil, fmt.Errorf("%s", i18n.T(e.cfg.Lang, "llm.err.embed_empty", "嵌入服务返回了空向量"))
		}
		vecs[i] = v
	}
	return vecs, nil
}

// normalize 原地把向量缩成单位长度，全零向量返回 false。
func normalize(v []float32) bool {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 || math.IsNaN(sum) || math.IsInf(sum, 0) {
		return false
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return true
}

// clipRunes 按字数截断，不会切在一个字符中间。
func clipRunes(s string, max int) string {
	if len(s) <= max { // 字节数不超就不可能超字数
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
