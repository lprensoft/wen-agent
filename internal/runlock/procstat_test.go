package runlock

import "testing"

// comm 里带空格和括号也不能影响字段定位。
func TestProcStartTicks(t *testing.T) {
	stat := "4321 (fancy (name) here) S 1 4321 4321 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 3 0 987654 1 0"
	ticks, ok := procStartTicks([]byte(stat))
	if !ok || ticks != 987654 {
		t.Fatalf("starttime = %d %v，想要 987654", ticks, ok)
	}
	if _, ok := procStartTicks([]byte("garbage")); ok {
		t.Fatal("坏内容不该解析成功")
	}
	if _, ok := procStartTicks([]byte("1 (x) S 1 2 3")); ok {
		t.Fatal("字段不够不该解析成功")
	}
}
