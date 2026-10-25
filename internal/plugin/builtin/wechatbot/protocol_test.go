package wechatbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
	"wen/internal/session"
)

func startBind(t *testing.T, f *fakeILink) *Plugin {
	t.Helper()
	sessDir, stateDir := t.TempDir(), t.TempDir()
	store, _ := session.NewStore(sessDir)
	p := New()
	if err := p.Init(testICtx(t, store, sessDir, echoTurn, stateDir), map[string]any{"api_base": f.srv.URL}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	if err := p.StartAction(context.Background(), "bind"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		st, _ := p.ActionState("bind")
		return st.Status == plugin.ActionPending && len(st.Image) > 0
	}, "等待二维码生成")
	return p
}

// 跨机房：扫码后服务端让我们换个地址继续查状态，之后的确认从新地址来
func TestBindFollowsRedirect(t *testing.T) {
	f := newFakeILink(t)
	other := newFakeILink(t)
	p := startBind(t, f)
	host := strings.TrimPrefix(other.srv.URL, "http://")
	f.qrStatus <- statusResponse{Status: "scaned_but_redirect", RedirectHost: host}
	other.qrStatus <- statusResponse{Status: "confirmed", BotToken: "TESTTOKEN", IlinkBotID: "bot-1",
		BaseURL: f.srv.URL, IlinkUserID: binderID}
	waitFor(t, func() bool {
		st, _ := p.ActionState("bind")
		return st.Status == plugin.ActionDone
	}, "等待在新地址上完成绑定")
	if got, _ := other.lastStatusHost.Load().(string); got != host {
		t.Errorf("应切到 %s 继续查询，实际 %q", host, got)
	}
}

// 需要输入验证码时如实报错，而不是一声不响地等到超时
func TestBindNeedVerifyCodeFailsClearly(t *testing.T) {
	f := newFakeILink(t)
	p := startBind(t, f)
	f.qrStatus <- statusResponse{Status: "need_verifycode"}
	waitFor(t, func() bool {
		st, _ := p.ActionState("bind")
		return st.Status == plugin.ActionError && strings.Contains(st.Message, "验证")
	}, "等待验证码提示")
}

func TestBindAlreadyBoundWithoutCreds(t *testing.T) {
	f := newFakeILink(t)
	p := startBind(t, f)
	f.qrStatus <- statusResponse{Status: "binded_redirect"}
	waitFor(t, func() bool {
		st, _ := p.ActionState("bind")
		return st.Status == plugin.ActionError && strings.Contains(st.Message, "已经绑定过")
	}, "等待已绑定提示")
}

// -14 之后的暂停期里发送也停下
func TestSendPausedAfterStaleToken(t *testing.T) {
	p, f, _, _ := newBound(t, echoTurn, "")
	p.pauseUntil(time.Now().Add(time.Hour))
	p.send(context.Background(), binderID, "在吗", "CTX")
	select {
	case m := <-f.sends:
		t.Fatalf("暂停期里不该发送: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
	p.pauseUntil(time.Time{})
	p.send(context.Background(), binderID, "在吗", "CTX")
	if m := f.expectSend(t); m.text != "在吗" {
		t.Fatalf("暂停结束后应恢复发送: %+v", m)
	}
}

// 消息 ID 超过 2^63 时整个响应也要解得开，否则游标不再前进、收信卡死
func TestLargeMessageIDParses(t *testing.T) {
	var resp getUpdatesResp
	raw := `{"ret":0,"msgs":[{"message_id":18446744073709551615,"from_user_id":"u"}],"get_updates_buf":"x"}`
	if err := json.Unmarshal([]byte(raw), &resp); err != nil || resp.Msgs[0].MessageID != 18446744073709551615 {
		t.Fatalf("大 ID 解析失败: %v %+v", err, resp)
	}
}

func TestClientVersionHeader(t *testing.T) {
	if got := encodeClientVersion("1.0.11"); got != 65547 {
		t.Errorf("1.0.11 应编码为 65547，得到 %d", got)
	}
	if got := encodeClientVersion("2.4.10"); got != 0x0002040A {
		t.Errorf("2.4.10 编码不对: %d", got)
	}
}

func TestFilterMarkdown(t *testing.T) {
	cases := map[string]string{
		"这是*重点*哦":                      "这是重点哦",
		"这是***特别***的":                  "这是特别的",
		"_强调_一下":                       "强调一下",
		"**粗体**照留":                     "**粗体**照留",
		"keep *english* italic":        "keep *english* italic",
		"##### 小标题":                    "小标题",
		"#### 四级照留":                    "#### 四级照留",
		"看图 ![猫](https://x/cat.png) 完": "看图  完",
		"代码 `*不动*` 里":                  "代码 `*不动*` 里",
		"snake_case_名字":                "snake_case_名字",
	}
	for in, want := range cases {
		if got := filterMarkdown(in); got != want {
			t.Errorf("%q → %q，应为 %q", in, got, want)
		}
	}
	fence := "```\n*代码里*\n##### 也不动\n```"
	if got := filterMarkdown(fence); got != fence {
		t.Errorf("代码块内不该动: %q", got)
	}
}

// 公共请求头：鉴权接口与扫码接口都带应用头
func TestAppHeadersOnAuthedRequests(t *testing.T) {
	var gotID, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotVer = r.Header.Get("iLink-App-Id"), r.Header.Get("iLink-App-ClientVersion")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := apiPost(context.Background(), srv.URL, "/x", map[string]any{"base_info": newBaseInfo()}, "T", time.Second); err != nil {
		t.Fatal(err)
	}
	if gotID != "bot" || gotVer != clientVersionHeader {
		t.Errorf("缺应用请求头: %q %q", gotID, gotVer)
	}
	if bi := newBaseInfo(); !strings.HasPrefix(bi.BotAgent, "Wen/") || bi.ChannelVersion != channelVersion {
		t.Errorf("base_info 不对: %+v", bi)
	}
}

// 开始收信时报上线，停下时报下线
func TestLifecycleNotices(t *testing.T) {
	p, f, _, _ := newBound(t, echoTurn, "")
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-f.lifecycle:
			if got != want {
				t.Fatalf("应收到 %s，实际 %s", want, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("没收到 %s", want)
		}
	}
	expect("notifystart")
	p.Stop()
	expect("notifystop")
}

// 按提示重新扫码绑定后，旧 token 引起的暂停不再拦着发送
func TestRebindClearsStalePause(t *testing.T) {
	f := newFakeILink(t)
	p := startBind(t, f)
	p.pauseUntil(time.Now().Add(time.Hour))
	f.qrStatus <- statusResponse{Status: "confirmed", BotToken: "TESTTOKEN", IlinkBotID: "bot-1",
		BaseURL: f.srv.URL, IlinkUserID: binderID}
	waitFor(t, func() bool {
		st, _ := p.ActionState("bind")
		return st.Status == plugin.ActionDone
	}, "等待重新绑定完成")
	if p.paused() {
		t.Fatal("换了凭证之后旧暂停应作废")
	}
	f.pushText(binderID, "在吗")
	if m := f.expectSend(t); m.text != "收到：在吗" {
		t.Fatalf("重新绑定后应能回复: %+v", m)
	}
}
