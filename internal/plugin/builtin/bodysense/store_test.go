package bodysense

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStoreRecordAccumulates(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, err := s.Record([]Touch{{Part: "手", Action: "牵"}}); err != nil {
		t.Fatalf("首次记录失败: %v", err)
	}
	out, err := s.Record([]Touch{{Part: "手", Action: "握"}})
	if err != nil {
		t.Fatalf("再次记录失败: %v", err)
	}
	if len(out) != 1 || out[0].Count != 2 {
		t.Fatalf("累计结果 = %+v，期望 1 条计数为 2", out)
	}
	if out[0].LastAct != "握" {
		t.Errorf("LastAct = %q，期望被后一次覆盖成「握」", out[0].LastAct)
	}
	if out[0].First.After(out[0].Last) {
		t.Error("First 不该晚于 Last")
	}
}

// 同一部位在一次调用里出现多次只算一次：计数没有回退手段，
// 一次幻觉不该能加上几十次。
func TestStoreRecordDedupesWithinCall(t *testing.T) {
	s := NewStore("", t.TempDir())
	out, err := s.Record([]Touch{{Part: "腰"}, {Part: "腰"}, {Part: "腰"}})
	if err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if len(out) != 1 || out[0].Count != 1 {
		t.Fatalf("结果 = %+v，期望只加 1 次", out)
	}
}

func TestStoreRecordKeepsExistingActWhenOmitted(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, err := s.Record([]Touch{{Part: "手", Action: "牵"}}); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	out, err := s.Record([]Touch{{Part: "手"}})
	if err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if out[0].LastAct != "牵" {
		t.Errorf("LastAct = %q，省略 action 时应保留原值", out[0].LastAct)
	}
}

func TestClipAct(t *testing.T) {
	// 这个字段由模型写入又逐轮进 system 消息，换行与超长都必须在入口挡掉
	if got := clipAct(" 轻轻\n地 握 "); got != "轻轻地握" {
		t.Errorf("clipAct = %q，期望压掉全部空白与换行", got)
	}
	long := strings.Repeat("握", maxActRunes+10)
	if got := clipAct(long); len([]rune(got)) != maxActRunes {
		t.Errorf("clipAct 长度 = %d，期望截到 %d", len([]rune(got)), maxActRunes)
	}
}

func TestStorePersistsAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "body")
	s := NewStore("", dir)
	if _, err := s.Record([]Touch{{Part: "肩"}}); err != nil {
		t.Fatalf("记录失败: %v", err)
	}

	// 换一个 Store 指向同一目录，读到的应当是落盘的内容
	got, err := NewStore("", dir).List()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 1 || got[0].Part != "肩" {
		t.Fatalf("读取结果 = %+v", got)
	}
	// 临时文件不该留下
	if _, err := os.Stat(filepath.Join(dir, "body.json.tmp")); !os.IsNotExist(err) {
		t.Error("原子写留下了 .tmp 文件")
	}
}

func TestStoreLoadRejectsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "body.json"), []byte("{不是数组"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore("", dir).List(); err == nil {
		t.Fatal("损坏的文件应当报错，而不是当成空库")
	}
}

func TestStoreClear(t *testing.T) {
	dir := t.TempDir()
	s := NewStore("", dir)
	if _, err := s.Record([]Touch{{Part: "手"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if got, _ := s.List(); len(got) != 0 {
		t.Fatalf("清空后仍有 %d 条", len(got))
	}
	// 再清一次不该报错
	if err := s.Clear(); err != nil {
		t.Errorf("重复清空报错: %v", err)
	}
}

func TestStoreConcurrentRecord(t *testing.T) {
	s := NewStore("", t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Record([]Touch{{Part: "手"}}); err != nil {
				t.Errorf("并发记录失败: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Count != 20 {
		t.Fatalf("并发累计 = %+v，期望 1 条计数为 20", got)
	}
}
