package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 端到端：用本地假提供商跑一遍 runEval，验证装配、后台结算、报告与 JSON。
func TestRunEvalEndToEndWithFakeProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var text strings.Builder
		for _, m := range body.Messages {
			text.WriteString(m.Content)
		}
		reply := "【笑】嗯，在呢。"
		if strings.Contains(text.String(), "same_person") {
			reply = `{"same_person":{"score":4,"reason":"一致"},"tone":{"score":4,"reason":"稳定"},"relation":{"score":5,"reason":"称呼没变"}}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		line, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": reply}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", line)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("lang: zh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	models, _ := json.Marshal(map[string]any{
		"version": 1,
		"providers": []any{map[string]any{
			"name": "fake", "type": "openai_compat",
			"base_url": srv.URL + "/v1", "api_key": "x",
			"models": []any{map[string]any{"id": "fake"}},
		}},
		"current": map[string]any{"provider": "fake", "model": "fake"},
	})
	if err := os.WriteFile(filepath.Join(dir, "models.json"), models, 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(dir, "script.json")
	script := `{"name":"e2e","start":"2026-03-01T08:00:00+08:00","turns":[{"say":"早"},{"say":"在吗"}],"max_background_turns":5}`
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dir, "report.md")
	jsonPath := filepath.Join(dir, "run.json")

	if err := runEval([]string{"-c", cfgPath, "-o", reportPath, "--json", jsonPath, scriptPath}); err != nil {
		t.Fatalf("runEval 失败: %v", err)
	}
	report, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# 回放评测：e2e", "| 像不像同一个人 | 4/5 |", "从 2026-03-01 08:00 开始", "## 后台轮次", "| agenda |"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("报告缺 %q", want)
		}
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var got evalJSON
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != evalJSONSchema || len(got.Turns) != 2 || got.Judge == nil || got.Judge.SamePerson != 4 {
		t.Fatalf("JSON 结果不对：%+v", got)
	}

	// 跨天示例：真跑一遍 run_hours、心跳、规划与日切，确认整条长时程链路可走通。
	weekReport := filepath.Join(dir, "week.md")
	weekJSON := filepath.Join(dir, "week.json")
	if err := runEval([]string{"-c", cfgPath, "-o", weekReport, "--json", weekJSON, "../../docs/eval/week.json"}); err != nil {
		t.Fatalf("跨天示例失败: %v", err)
	}
	week, err := os.ReadFile(weekReport)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# 回放评测：跨天的一周", "## 后台轮次", "| 1 | 早，今天打算做什么？"} {
		if !strings.Contains(string(week), want) {
			t.Errorf("跨天报告缺 %q", want)
		}
	}
}
