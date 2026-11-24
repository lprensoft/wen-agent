package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wen/internal/agent"
	"wen/internal/config"
	"wen/internal/llm"
	"wen/internal/modelcfg"
	"wen/internal/plugin"
	"wen/internal/session"
	"wen/internal/usage"
)

// modelsDocJSON 造一份模型配置：p1/mA 的单价由参数给，p2/mB 固定 1/2 且没有 API Key
// （用来覆盖 applyCurrentModel 失败的那条路径）。current 由调用方指定。
func modelsDocJSON(input, output float64, provider, model string) string {
	return fmt.Sprintf(`{
  "version": 1,
  "providers": [
    {
      "name": "p1",
      "type": "openai_compat",
      "base_url": "https://api.example.com",
      "api_key": "sk-test",
      "models": [{"id": "mA", "price": {"currency": "CNY", "input": %g, "output": %g}}]
    },
    {
      "name": "p2",
      "type": "openai_compat",
      "base_url": "https://api2.example.com",
      "models": [{"id": "mB", "price": {"currency": "CNY", "input": 1, "output": 2}}]
    }
  ],
  "current": {"provider": %q, "model": %q}
}`, input, output, provider, model)
}

// saveModelsTestServer 造一台能保存模型配置的 Server：临时 models.json、一条本月
// 账单（p1/mA 一百万输入 token）、按它价格算的预算，以及 applyCurrentModel 需要的 Agent。
func saveModelsTestServer(t *testing.T, doc string) *Server {
	t.Helper()
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.json")
	if err := os.WriteFile(modelsPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := modelcfg.NewStore("", modelsPath, &config.Config{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ledger, err := usage.Open(filepath.Join(dir, "usage"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(usage.Entry{TS: time.Now(), Kind: usage.KindTurn,
		Provider: "p1", Model: "mA", Prompt: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	budget := usage.NewBudget(usage.DefaultSettings(), ledger, models.Price, time.Now())

	provider, err := llm.New(llm.Config{
		Type: llm.TypeOpenAICompat, BaseURL: "https://api.example.com", APIKey: "sk-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(provider, plugin.NewManager(plugin.InitContext{}, ""), store, agent.Options{Model: "mA"})
	return &Server{agent: ag, models: models, usage: ledger, usageBudget: budget}
}

// closeTo 比较金额：单价是浮点数，千万分之一以下的差都是浮点噪声。
func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-6 }

// 保存模型后预算要按新价重扫：本月已花金额是按旧价算的，不重扫就得等跨月才发现
// 上限的真实位置。
func TestSaveModelsRefreshesBudget(t *testing.T) {
	s := saveModelsTestServer(t, modelsDocJSON(1, 2, "p1", "mA"))
	now := time.Now()
	if st := s.usageBudget.Status(now); !closeTo(st.SpentCost, 1) {
		t.Fatalf("初始金额应按 1/百万算成 1.00: %+v", st)
	}

	rec := httptest.NewRecorder()
	s.saveModels(rec, httptest.NewRequest(http.MethodPut, "/api/models",
		strings.NewReader(modelsDocJSON(2, 4, "p1", "mA"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if st := s.usageBudget.Status(now); !closeTo(st.SpentCost, 2) {
		t.Fatalf("单价翻倍后预算应按新价算成 2.00: %+v", st)
	}
}

// 脱敏视图 → 改名 → 整档保存的往返：两家旧配置的重名提供商（Ζευς / ΖΕΥΣ）密钥
// 必须各回各家。界面只拿得到掩码（api_key 留空），服务端靠视图下发的 ref 认出
// 被编辑的那一条；少了 ref 就会按名字 EqualFold 配对，第一家的密钥被清空、第二家
// 反而被写入第一家的密钥，凭据可能发往另一家的地址。
func TestSaveModelsMaskedRoundTripKeepsKeys(t *testing.T) {
	s := saveModelsTestServer(t, legacyGreekModelsDoc)

	// 与 GET /api/models 同形：只有掩码，没有明文密钥。
	raw, err := json.Marshal(s.modelsView(""))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Providers []struct {
			Ref         string           `json:"ref"`
			Name        string           `json:"name"`
			Type        string           `json:"type"`
			BaseURL     string           `json:"base_url"`
			APIKey      string           `json:"api_key"`
			Dialect     string           `json:"thinking_dialect"`
			Protocol    string           `json:"protocol"`
			PromptCache *bool            `json:"prompt_cache"`
			Models      []modelcfg.Model `json:"models"`
		} `json:"providers"`
		Current   modelcfg.Selection   `json:"current"`
		Fallbacks []modelcfg.Selection `json:"fallbacks"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Providers) != 2 {
		t.Fatalf("providers = %d", len(payload.Providers))
	}
	for i := range payload.Providers {
		if payload.Providers[i].Ref == "" {
			t.Fatalf("视图应下发 ref，界面才能把条目认回来: %+v", payload.Providers[i])
		}
		payload.Providers[i].APIKey = "" // 界面留空 = 沿用旧值
	}
	payload.Providers[0].Name = "Zeus"
	payload.Current.Provider = "Zeus"

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.saveModels(rec, httptest.NewRequest(http.MethodPut, "/api/models", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	keys := map[string]string{}
	for _, p := range s.models.Snapshot().Providers {
		keys[p.Name] = p.APIKey
	}
	if keys["Zeus"] != "sk-a" || keys["ΖΕΥΣ"] != "sk-b" {
		t.Fatalf("改名保存后密钥应对应原来的条目: %+v", keys)
	}

	// 同一份请求重试（首次成功但响应丢了）：旧 ref 已对不上，必须拒绝，磁盘上的
	// 密钥原样保留。
	rec = httptest.NewRecorder()
	s.saveModels(rec, httptest.NewRequest(http.MethodPut, "/api/models", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("重试失效的 ref 应被拒绝: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, p := range s.models.Snapshot().Providers {
		if p.APIKey == "" {
			t.Fatalf("被拒绝的重试清掉了 %q 的密钥", p.Name)
		}
	}
}

// 测试连接沿用已保存的密钥时同样只认 ref：旧配置里两家只差大小写、模型 ID 相同，
// 只按名字找会取到第一家的密钥发往第二家的地址。分不清（不带 ref）或对不上时，
// 一个请求都不能发出去。
func TestTestModelPicksSavedKeyByRef(t *testing.T) {
	var (
		mu   sync.Mutex
		auth string
		hits int
	)
	held := saveModelsTestServer(t, legacyGreekModelsDoc)
	second := held.models.Snapshot().Providers[1].Ref

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth, hits = r.Header.Get("Authorization"), hits+1
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	}))
	defer target.Close()

	post := func(ref string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]any{
			"provider": "ΖΕΥΣ", "ref": ref, "type": llm.TypeOpenAICompat,
			"base_url": target.URL, "model": "m",
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		held.testModel(rec, httptest.NewRequest(http.MethodPost, "/api/models/test", bytes.NewReader(body)))
		return rec
	}

	for _, ref := range []string{"", "gone"} {
		if rec := post(ref); rec.Code != http.StatusBadRequest {
			t.Fatalf("ref=%q 应在发请求前被拒绝: status = %d, body = %s", ref, rec.Code, rec.Body.String())
		}
	}
	mu.Lock()
	before := hits
	mu.Unlock()
	if before != 0 {
		t.Fatalf("身份没认定就发出了 %d 个请求", before)
	}

	if rec := post(second); rec.Code != http.StatusOK {
		t.Fatalf("带第二家的 ref 应通过: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != before+1 || auth != "Bearer sk-b" {
		t.Fatalf("应带着第二家自己的密钥发一次请求: hits = %d, auth = %q", hits, auth)
	}
}

const legacyGreekModelsDoc = `{
  "version": 1,
  "providers": [
    {"name":"Ζευς","type":"openai_compat","base_url":"https://a.example.com","api_key":"sk-a",
     "models":[{"id":"m"}]},
    {"name":"ΖΕΥΣ","type":"openai_compat","base_url":"https://b.example.com","api_key":"sk-b",
     "models":[{"id":"m"}]}
  ],
  "current": {"provider":"Ζευς","model":"m"}
}`

// 新模型因为缺 API Key 应用失败时，价格也已经落盘了：预算更不该继续按旧价算，
// 否则上限看着绰绰有余、实际早该提醒。重扫因此必须排在 applyCurrentModel 之前。
func TestSaveModelsRefreshesBudgetWhenApplyFails(t *testing.T) {
	s := saveModelsTestServer(t, modelsDocJSON(1, 2, "p1", "mA"))
	now := time.Now()

	// 新当前模型落在没有密钥的 p2 上：Save 成功，applyCurrentModel 必然失败。
	rec := httptest.NewRecorder()
	s.saveModels(rec, httptest.NewRequest(http.MethodPut, "/api/models",
		strings.NewReader(modelsDocJSON(2, 4, "p2", "mB"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 Key 应让应用失败: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if st := s.usageBudget.Status(now); !closeTo(st.SpentCost, 2) {
		t.Fatalf("应用失败时预算也应按新价重扫: %+v", st)
	}
}
