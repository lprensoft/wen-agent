package imbot

import (
	"context"
	"testing"
	"time"

	"wen/internal/plugin"
)

func TestParseWhitelist(t *testing.T) {
	cfg := map[string]any{"whitelist": " u1 \n\n u2\r\n\tu3\n"}
	got := ParseWhitelist(cfg)
	for _, id := range []string{"u1", "u2", "u3"} {
		if !got[id] {
			t.Errorf("白名单缺少 %q: %#v", id, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("白名单条数 = %d，想要 3: %#v", len(got), got)
	}
	if len(ParseWhitelist(nil)) != 0 {
		t.Error("没有配置时白名单应为空")
	}
}

func TestConfirmTimeoutFieldAndConfig(t *testing.T) {
	f := ConfirmTimeoutField(0)
	if f.Key != "confirm_timeout_sec" || f.Default != DefaultConfirmTimeoutSec {
		t.Errorf("默认确认超时字段不对: %+v", f)
	}
	if got := ConfirmTimeout(nil); got != DefaultConfirmTimeoutSec*time.Second {
		t.Errorf("默认确认超时 = %v, 想要 %v", got, DefaultConfirmTimeoutSec*time.Second)
	}
	if got := ConfirmTimeout(map[string]any{"confirm_timeout_sec": 42}); got != 42*time.Second {
		t.Errorf("配置确认超时 = %v, 想要 42s", got)
	}
}

func TestOutputFields(t *testing.T) {
	fields := OutputFields()
	want := []string{"show_thinking", "show_tools", "push_notices"}
	if len(fields) != len(want) {
		t.Fatalf("输出字段数 = %d, 想要 %d", len(fields), len(want))
	}
	for i, key := range want {
		if fields[i].Key != key {
			t.Errorf("第 %d 个字段 = %q, 想要 %q", i, fields[i].Key, key)
		}
	}
}

func TestNewCoreReadsSharedConfig(t *testing.T) {
	ictx := plugin.InitContext{
		Lang:     "zh",
		StateDir: t.TempDir(),
		RunTurn:  func(context.Context, string, string) (string, error) { return "", nil },
		NewSession: func() (string, error) {
			return "s1", nil
		},
		Compact:  func(context.Context, string) error { return nil },
		Status:   func(string) (plugin.StatusInfo, error) { return plugin.StatusInfo{}, nil },
		Sessions: fakeSessions{},
	}
	cfg := map[string]any{
		"confirm_timeout_sec": 42,
		"show_thinking":       true,
		"show_tools":          true,
		"push_notices":        true,
		"merge_window_sec":    7,
		"human_pace":          true,
		"busy_hold":           true,
		"wake_after_minutes":  9,
	}
	allowed := map[string]bool{"u1": true}
	core, err := NewCore(CoreOptions{
		PluginName: "test_bot",
		Sender:     newFakeSender(),
		Ctx:        ictx,
		Cfg:        cfg,
		Allow:      func(id string) bool { return allowed[id] },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Stop()

	got := core.cfg
	if got.ConfirmTimeout != 42*time.Second {
		t.Errorf("ConfirmTimeout = %v, 想要 42s", got.ConfirmTimeout)
	}
	if !got.ShowThinking || !got.ShowTools || !got.PushNotices {
		t.Errorf("输出开关没读全: %+v", got)
	}
	if got.MergeWindow != 7*time.Second || !got.HumanPace {
		t.Errorf("收发节奏没读全: %+v", got)
	}
	if !got.HoldWhenBusy || got.WakeAfter != 9*time.Minute {
		t.Errorf("忙时晚点回没读全: %+v", got)
	}
	if got.Lang != "zh" || got.StateDir != ictx.StateDir {
		t.Errorf("语言或状态目录没带上: %+v", got)
	}
	if got.TurnTimeout != DefaultTurnTimeout {
		t.Errorf("TurnTimeout = %v, 想要默认 %v", got.TurnTimeout, DefaultTurnTimeout)
	}
	if !core.allowed("u1") || core.allowed("u2") {
		t.Error("Allow 回调没有生效")
	}
}
