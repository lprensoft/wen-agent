package availability

import (
	"testing"
	"time"
)

func reset() {
	mu.Lock()
	states = nil
	subs = map[int]func(){}
	mu.Unlock()
}

// Set 覆盖同源记录；Current 取未过期里档位最高的一条。
func TestSetOverridesSameSource(t *testing.T) {
	reset()
	t.Cleanup(reset)
	now := time.Now()
	Set(State{Source: "a", Activity: "跑步", Level: Light, Until: now.Add(time.Hour)})
	Set(State{Source: "a", Activity: "查资料", Level: Heavy, Until: now.Add(2 * time.Hour)})
	Set(State{Source: "b", Activity: "开会", Level: Light, Until: now.Add(time.Hour)})

	cur, ok := Current(now)
	if !ok || cur.Source != "a" || cur.Activity != "查资料" || cur.Level != Heavy {
		t.Fatalf("应取 a 的最新一条（重忙），得到 %+v ok=%v", cur, ok)
	}
	mu.Lock()
	n := len(states)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("每个来源只留一条，得到 %d 条", n)
	}
}

// Until 过期后 Current 不再返回它；Until 不在未来的写入被丢弃。
func TestExpiry(t *testing.T) {
	reset()
	t.Cleanup(reset)
	now := time.Now()
	Set(State{Source: "a", Activity: "跑步", Level: Light, Until: now.Add(time.Minute)})
	if _, ok := Current(now); !ok {
		t.Fatal("未过期时应能取到")
	}
	if _, ok := Current(now.Add(2 * time.Minute)); ok {
		t.Fatal("过期后不应再取到")
	}
	Set(State{Source: "b", Activity: "过去的事", Level: Heavy, Until: now.Add(-time.Minute)})
	if _, ok := Current(now); ok {
		t.Fatal("Until 不在未来的写入应被丢弃")
	}
	Set(State{Source: "c", Activity: "空闲", Level: Free, Until: now.Add(time.Hour)})
	if _, ok := Current(now); ok {
		t.Fatal("空闲档位不占记录")
	}
}

// Clear 撤掉记录并叫醒读取方；Set 同样叫醒。
func TestClearAndNotify(t *testing.T) {
	reset()
	t.Cleanup(reset)
	calls := 0
	cancel := Subscribe(func() { calls++ })
	defer cancel()
	now := time.Now()
	Set(State{Source: "a", Activity: "跑步", Level: Light, Until: now.Add(time.Hour)})
	if calls != 1 {
		t.Fatalf("Set 应叫醒一次，得到 %d", calls)
	}
	Clear("a")
	if calls != 2 {
		t.Fatalf("Clear 应叫醒一次，得到 %d", calls)
	}
	if _, ok := Current(now); ok {
		t.Fatal("Clear 后不应再有记录")
	}
	Clear("a") // 没有记录时不叫醒
	if calls != 2 {
		t.Fatalf("撤回不存在的记录不该叫醒，得到 %d", calls)
	}
}

func TestLevelNames(t *testing.T) {
	for _, l := range []Level{Free, Light, Heavy, Away} {
		got, ok := ParseLevel(l.String())
		if !ok || got != l {
			t.Fatalf("%v 往返失败：%v %v", l, got, ok)
		}
	}
	if _, ok := ParseLevel("很忙"); ok {
		t.Fatal("不认识的说法不该解析成功")
	}
	if Level(9).String() != "未知" {
		t.Fatal("越界档位应为「未知」")
	}
}

// 写入一侧不看真实时钟：调用方按自己的时钟写下的状态，要按调用方的「现在」判断有效性。
// 日程插件整套跑在可注入的时钟上，「今天 16:30 结束」这条状态不该因为真实时间过了
// 16:30 就写不进去（那曾表现为一到下午整包测试就失败）。
func TestSetDoesNotJudgeByWallClock(t *testing.T) {
	reset()
	t.Cleanup(reset)
	past := time.Now().Add(-2 * time.Hour)
	Set(State{Source: "a", Activity: "两小时前那场会", Level: Heavy,
		Since: past.Add(-time.Hour), Until: past})

	// 按调用方的时钟，那时它还没结束
	if st, ok := Current(past.Add(-time.Minute)); !ok || st.Activity != "两小时前那场会" {
		t.Fatalf("按调用方的「现在」应当取得到: %+v ok=%v", st, ok)
	}
	// 按此刻读，它早就过去了——过期由读取一侧判定
	if _, ok := Current(time.Now()); ok {
		t.Fatal("按此刻读应当已过期")
	}
}

// 两个订阅方都要被叫醒，撤销之后不再叫；CurrentExcept 不看被排除的写入方。
func TestSubscribersAndExcept(t *testing.T) {
	reset()
	t.Cleanup(reset)
	a, b := 0, 0
	cancelA := Subscribe(func() { a++ })
	cancelB := Subscribe(func() { b++ })
	defer cancelB()
	now := time.Now()
	Set(State{Source: "heartbeat", Activity: "睡觉", Level: Away, Until: now.Add(time.Hour)})
	if a != 1 || b != 1 {
		t.Fatalf("两个订阅方都该被叫醒：a=%d b=%d", a, b)
	}
	cancelA()
	cancelA() // 重复撤销无害
	Set(State{Source: "agenda", Activity: "图书馆", Level: Light, Until: now.Add(time.Hour)})
	if a != 1 || b != 2 {
		t.Fatalf("撤销后不该再叫：a=%d b=%d", a, b)
	}
	if st, ok := CurrentExcept(now, "heartbeat"); !ok || st.Source != "agenda" {
		t.Errorf("排除自己后应看到 agenda：%+v %v", st, ok)
	}
	if st, _ := Current(now); st.Source != "heartbeat" {
		t.Errorf("不排除时取档位最高的：%+v", st)
	}
}
