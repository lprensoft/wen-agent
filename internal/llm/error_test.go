package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIErrorText(t *testing.T) {
	withStatus := &APIError{Status: 429, Body: "rate limited"}
	if got := withStatus.Error(); got != "llm api: status 429: rate limited" {
		t.Errorf("Error() = %q", got)
	}
	// 流内错误帧没有状态码，文本形式与定型前保持一致
	inStream := &APIError{Body: "overloaded: 服务过载"}
	if got := inStream.Error(); got != "llm api: overloaded: 服务过载" {
		t.Errorf("Error() = %q", got)
	}
}

func TestIsConfigError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"401 密钥错误", &APIError{Status: 401, Body: "bad key"}, true},
		{"403 无权限", &APIError{Status: 403, Body: "forbidden"}, true},
		{"404 地址或模型名错误", &APIError{Status: 404, Body: "not found"}, true},
		{"400 一般请求错误", &APIError{Status: 400, Body: "bad request"}, false},
		{"429 限流", &APIError{Status: 429, Body: "rate limited"}, false},
		{"500 服务端错误", &APIError{Status: 500, Body: "oops"}, false},
		{"流内错误帧", &APIError{Body: "overloaded"}, false},
		{"包装后仍可识别", fmt.Errorf("wrap: %w", &APIError{Status: 401, Body: "x"}), true},
		{"非 APIError", errors.New("dial tcp: refused"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := IsConfigError(c.err); got != c.want {
			t.Errorf("%s: IsConfigError = %v, want %v", c.name, got, c.want)
		}
	}
}

// collectErr 读完流并返回收到的错误事件（没有则为 nil）。
func collectErr(t *testing.T, ch <-chan StreamEvent) error {
	t.Helper()
	var got error
	for ev := range ch {
		if ev.Type == EventError {
			got = ev.Err
		}
	}
	return got
}

func TestOpenAICompatNon200IsTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-bad", "")
	_, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: testMessages("hi")})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("err = %v, want APIError{Status:401}", err)
	}
}

// 流中途的错误帧必须成为错误事件，而不是被当成无法识别的帧静默跳过
// （那会表现成「空回复且正常结束」，用户侧完全无感）。
func TestOpenAICompatStreamErrorFrame(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"说到一半"}}]}`,
		`{"error":{"type":"server_error","message":"内部错误"}}`,
	}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: testMessages("hi")})
	if err != nil {
		t.Fatal(err)
	}
	streamErr := collectErr(t, ch)
	var ae *APIError
	if !errors.As(streamErr, &ae) {
		t.Fatalf("stream err = %v, want APIError", streamErr)
	}
	if !strings.Contains(ae.Body, "server_error") || !strings.Contains(ae.Body, "内部错误") {
		t.Errorf("Body = %q", ae.Body)
	}
}

// code 为数字的错误帧（部分厂商如此）不能连累整帧解析失败。
func TestOpenAICompatStreamErrorFrameNumericCode(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{`{"error":{"code":20015,"message":"内容不合规"}}`}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: testMessages("hi")})
	if err != nil {
		t.Fatal(err)
	}
	streamErr := collectErr(t, ch)
	var ae *APIError
	if !errors.As(streamErr, &ae) {
		t.Fatalf("stream err = %v, want APIError", streamErr)
	}
	if !strings.Contains(ae.Body, "20015") || !strings.Contains(ae.Body, "内容不合规") {
		t.Errorf("Body = %q", ae.Body)
	}
}

func TestOpenAICompatContentFilterFinish(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"开头"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
	}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: testMessages("hi")})
	if err != nil {
		t.Fatal(err)
	}
	streamErr := collectErr(t, ch)
	var ae *APIError
	if !errors.As(streamErr, &ae) || ae.Kind != KindContentFilter {
		t.Fatalf("stream err = %v, want APIError{Kind:content_filter}", streamErr)
	}
}

// 正常的 finish_reason（stop / tool_calls）不该被错误分支误伤。
func TestOpenAICompatNormalFinishReason(t *testing.T) {
	var got map[string]any
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"好的"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}, &got)
	defer srv.Close()

	p := NewOpenAICompat("", srv.URL, "sk-test", "")
	ch, err := p.ChatStream(context.Background(), ChatRequest{Model: "m", Messages: testMessages("hi")})
	if err != nil {
		t.Fatal(err)
	}
	content, _, _ := collect(t, ch)
	if content != "好的" {
		t.Errorf("content = %q", content)
	}
}
