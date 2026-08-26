package mood

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func TestApplyAccumulates(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, _, err := s.Apply(20, 30, 0, "被夸了", base); err != nil {
		t.Fatal(err)
	}
	st, applied, err := s.Apply(-5, 30, 0, "又被打断", base)
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != 15 || applied != -5 {
		t.Fatalf("结果 = %+v applied=%d，期望 15 / -5", st, applied)
	}
	if st.Reason != "又被打断" {
		t.Errorf("起因 = %q，期望被后一次覆盖", st.Reason)
	}
}

func TestApplyClampsDeltaAndRange(t *testing.T) {
	s := NewStore("", t.TempDir())
	// 单次幅度上限
	st, applied, err := s.Apply(500, 30, 0, "很离谱", base)
	if err != nil {
		t.Fatal(err)
	}
	if applied != 30 || st.Value != 30 {
		t.Fatalf("单次上限未生效: %+v applied=%d", st, applied)
	}
	// 量程上限：连续加也不该越过 +100
	for range 10 {
		if _, _, err := s.Apply(30, 30, 0, "还在加", base); err != nil {
			t.Fatal(err)
		}
	}
	st, _, err = s.Apply(30, 30, 0, "再加", base)
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != moodMax {
		t.Fatalf("值 = %d，期望封顶在 %d", st.Value, moodMax)
	}
}

func TestApplyRejectsZeroDelta(t *testing.T) {
	if _, _, err := NewStore("", t.TempDir()).Apply(0, 30, 0, "什么也没发生", base); err == nil {
		t.Fatal("变化量为 0 应当报错")
	}
}

func TestDecayTowardCalm(t *testing.T) {
	cases := []struct {
		name    string
		value   int
		perHour int
		elapsed time.Duration
		want    int
	}{
		{"正向回落", 60, 5, 4 * time.Hour, 40},
		{"负向回落", -60, 5, 4 * time.Hour, -40},
		{"回落到平静就停住", 20, 5, 100 * time.Hour, 0},
		{"负向也不越过平静", -20, 5, 100 * time.Hour, 0},
		{"速率为 0 不回落", 60, 0, 100 * time.Hour, 60},
		{"不足一点不回落", 60, 5, 5 * time.Minute, 60},
		{"时钟倒流原样返回", 60, 5, -10 * time.Hour, 60},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := State{Value: c.value, Reason: "起因", Updated: base}
			got := decay(st, c.perHour, base.Add(c.elapsed))
			if got.Value != c.want {
				t.Errorf("回落后 = %d，期望 %d", got.Value, c.want)
			}
			// 心情都平了，那件事就不该再挂在提示词里
			if got.Value == 0 && got.Reason != "" {
				t.Errorf("回到平静后起因应当一并丢掉，得到 %q", got.Reason)
			}
		})
	}
}

// 时间戳缺失时不猜：原样返回，不按「现在」算一个回落量出来。
func TestDecaySkipsZeroTimestamp(t *testing.T) {
	got := decay(State{Value: 50}, 5, base)
	if got.Value != 50 {
		t.Errorf("值 = %d，期望原样返回", got.Value)
	}
}

// 折算必须在叠加之前：不然隔了一天再调整，会从一天前那个没回落的值上继续加。
func TestApplyDecaysBeforeAdding(t *testing.T) {
	s := NewStore("", t.TempDir())
	if _, _, err := s.Apply(60, 100, 5, "很开心", base); err != nil {
		t.Fatal(err)
	}
	st, _, err := s.Apply(10, 100, 5, "又有点开心", base.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != 50 { // 60 - 20（回落）+ 10
		t.Fatalf("值 = %d，期望 50（先回落到 40 再加 10）", st.Value)
	}
}

// 读取路径不回写：读一次写一次盘没有必要，也会让并发的读覆盖彼此的时间戳。
func TestCurrentDoesNotPersistDecay(t *testing.T) {
	dir := t.TempDir()
	s := NewStore("", dir)
	if _, _, err := s.Apply(60, 100, 5, "很开心", base); err != nil {
		t.Fatal(err)
	}
	if st, _, err := s.Current(5, base.Add(4*time.Hour)); err != nil || st.Value != 40 {
		t.Fatalf("读到 %+v err=%v，期望折算成 40", st, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mood.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"value": 60`) {
		t.Errorf("盘上的原值被读取路径改写了:\n%s", raw)
	}
}

func TestCurrentReportsNoRecord(t *testing.T) {
	if _, ok, err := NewStore("", t.TempDir()).Current(5, base); err != nil || ok {
		t.Fatalf("空库应当报告没有记录，得到 ok=%v err=%v", ok, err)
	}
}

func TestLoadRejectsBrokenFileAndClampsRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mood.json")
	if err := os.WriteFile(path, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewStore("", dir).Current(0, base); err == nil {
		t.Fatal("损坏的文件应当报错")
	}
	// 手改过头的值要收回量程内，而不是原样用出去
	if err := os.WriteFile(path, []byte(`{"value": 9999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _, err := NewStore("", dir).Current(0, base)
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != moodMax {
		t.Errorf("值 = %d，期望收到 %d", st.Value, moodMax)
	}
}

func TestClipReason(t *testing.T) {
	if got := clipReason(" 她夸了\n新剪的  头发 "); got != "她夸了 新剪的 头发" {
		t.Errorf("clipReason = %q，期望压掉换行与连续空白", got)
	}
	long := strings.Repeat("很", maxReasonRunes+10)
	if got := clipReason(long); len([]rune(got)) != maxReasonRunes {
		t.Errorf("长度 = %d，期望截到 %d", len([]rune(got)), maxReasonRunes)
	}
}

func TestClearAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s := NewStore("", dir)
	if _, _, err := s.Apply(40, 100, 0, "开心", base); err != nil {
		t.Fatal(err)
	}
	// 换一个 Store 指向同一目录，读到的应当是落盘的内容
	if st, ok, err := NewStore("", dir).Current(0, base); err != nil || !ok || st.Value != 40 {
		t.Fatalf("落盘读取 = %+v ok=%v err=%v", st, ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mood.json.tmp")); !os.IsNotExist(err) {
		t.Error("原子写留下了 .tmp 文件")
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Current(0, base); ok {
		t.Error("清空后仍读到记录")
	}
	if err := s.Clear(); err != nil {
		t.Errorf("重复清空报错: %v", err)
	}
}

func TestConcurrentApply(t *testing.T) {
	s := NewStore("", t.TempDir())
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.Apply(1, 30, 0, "一点点", base); err != nil {
				t.Errorf("并发调整失败: %v", err)
			}
		}()
	}
	wg.Wait()
	st, _, err := s.Current(0, base)
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != 20 {
		t.Fatalf("并发累计 = %d，期望 20", st.Value)
	}
}
