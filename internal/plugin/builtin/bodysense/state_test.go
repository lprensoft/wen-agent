package bodysense

import (
	"testing"
	"time"

	"wen/internal/plugin"
)

// 此刻的身体与累计接触分成两块；数值与次数都按运行时事实原样给出。
func TestStateListsBodyAndContacts(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	if _, _, _, err := p.stateStoreFor("").Apply(30, 20, 30, 30, 10, "跑完步", now); err != nil {
		t.Fatal(err)
	}
	if _, err := p.storeFor("").RecordAt([]Touch{{Part: "手", Action: "握手"}}, now); err != nil {
		t.Fatal(err)
	}

	secs := p.State(plugin.NewStateQuery("en", false, now))
	if len(secs) != 2 {
		t.Fatalf("块数 = %d，期望身体与接触两块", len(secs))
	}
	body, contacts := secs[0], secs[1]
	if body.Key != "body" || len(body.Rows) != 3 {
		t.Fatalf("身体块 = %+v", body)
	}
	if body.Rows[0].Label != "Arousal" || body.Rows[0].Value != "30" {
		t.Errorf("唤起行 = %+v", body.Rows[0])
	}
	if body.Rows[2].Value != "跑完步" {
		t.Errorf("起因行 = %+v", body.Rows[2])
	}
	if contacts.Key != "contacts" || contacts.Summary != "1 parts touched" || len(contacts.Rows) != 1 {
		t.Fatalf("接触块 = %+v", contacts)
	}
	if contacts.Rows[0].Value != "手" || contacts.Rows[0].Meta != "1 time(s)" {
		t.Errorf("接触行 = %+v", contacts.Rows[0])
	}
}

func TestStateEmptyAndPinned(t *testing.T) {
	p := newTestPlugin(t, nil)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	secs := p.State(plugin.NewStateQuery("zh", false, now))
	if len(secs) != 1 || secs[0].Empty == "" {
		t.Fatalf("空态 = %+v", secs)
	}
	if got := p.State(plugin.NewStateQuery("zh", true, now)); got != nil {
		t.Errorf("pinned 查询应返回 nil，得到 %+v", got)
	}
}
