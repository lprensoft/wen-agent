package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wen/internal/config"
	"wen/internal/runlock"
)

// 离线写入期间必须握着运行权：服务不能在这时启动，把用户刚保存的改动盖回去。
func TestWriteBackendHoldsRunLock(t *testing.T) {
	base := t.TempDir()
	cfg, err := config.Load(filepath.Join(base, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := openBackendForWrite(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runlock.Lock(base); !errors.Is(err, runlock.ErrBusy) {
		release()
		t.Fatalf("离线写入没有持锁：%v", err)
	}
	release()
	inst, err := runlock.Lock(base)
	if err != nil {
		t.Fatalf("收尾之后运行权没有放开：%v", err)
	}
	inst.Release()
}

// 服务已经持有运行权、但接口还没应答（启动中或探活超时）：不能退回离线写入。
func TestWriteBackendRefusesWhileRunLockHeld(t *testing.T) {
	base := t.TempDir()
	cfg, err := config.Load(filepath.Join(base, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := runlock.Lock(base)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Release()
	// 登记指向一个没人应答的端口，模拟「服务已持锁、HTTP 还没起来」。
	if err := inst.Register(runlock.Info{
		PID: os.Getpid(), Addr: "127.0.0.1:1", Started: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openBackendForWrite(cfg); err == nil {
		t.Fatal("运行权还在别人手里时不该给出可写的后端")
	}
}

// 旧版服务只写登记、不持运行权：登记核实过它在运行，但接口暂时不应答时，这里仍能
// 取到运行锁——不能因此离线写，改动会被那个服务覆盖。
func TestWriteBackendRefusesRunningButUnreachable(t *testing.T) {
	base := t.TempDir()
	cfg, err := config.Load(filepath.Join(base, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"pid":%d,"addr":"127.0.0.1:1","started":%q}`,
		os.Getpid(), time.Now().Format(time.RFC3339))
	if err := os.WriteFile(runlock.Path(base), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openBackendForWrite(cfg); err == nil {
		t.Fatal("实例还在运行（只是没应答）时不该给出可写的后端")
	}
	// 只读入口退回离线读本地状态，不连那个地址。
	if b, err := openBackend(cfg); err != nil || b == nil {
		t.Fatalf("只读入口应当退回离线模式：%v", err)
	}
}

// 登记判不出身份时，可写的后端必须拒绝：退回离线写状态文件会被运行中的服务覆盖。
func TestWriteBackendRefusesUnverifiableInstance(t *testing.T) {
	base := t.TempDir()
	cfg, err := config.Load(filepath.Join(base, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	inst, err := runlock.Lock(base)
	if err != nil {
		t.Fatal(err)
	}
	// 没有 started：无法核对进程创建时间，身份判不出来。
	if err := inst.Register(runlock.Info{PID: os.Getpid(), Addr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	// 模拟接手中断：登记停在 .releasing。
	if err := os.Rename(runlock.Path(base), runlock.Path(base)+".releasing"); err != nil {
		t.Fatal(err)
	}
	inst.Release()

	if _, state := runlock.ReadState(base); state != runlock.StateUnknown {
		t.Fatalf("准备出来的状态是 %v，想要 StateUnknown", state)
	}
	if _, _, err := openBackendForWrite(cfg); err == nil {
		t.Fatal("身份无法核实时不该给出可写的后端")
	}
	if b, err := openBackend(cfg); err != nil || b == nil {
		t.Fatalf("只读入口应当退回离线模式：%v", err)
	}
}
