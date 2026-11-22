package runlock

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLockIsExclusivePerDirectory(t *testing.T) {
	dir := t.TempDir()
	inst, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Register(Info{PID: 1, Addr: "127.0.0.1:8090"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("第二个实例没有被拦住：%v", err)
	}
	info, ok := readInfo(Path(dir))
	if !ok || info.Addr != "127.0.0.1:8090" {
		t.Fatalf("第二个实例改写了登记：%+v %v", info, ok)
	}

	inst.Release()
	inst.Release() // 重复调用不该出问题
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("释放后登记文件还在：%v", err)
	}
	again, err := Lock(dir)
	if err != nil {
		t.Fatalf("前一个实例退出后仍取不到运行权：%v", err)
	}
	defer again.Release()
}

// 运行权还在别人手里时，配置工具不能当成「没人管这份目录」去离线写。
func TestInUseFollowsRunLock(t *testing.T) {
	dir := t.TempDir()
	if InUse(dir) {
		t.Fatal("没有实例时不该报占用")
	}
	inst, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !InUse(dir) {
		t.Fatal("运行权被持有时应当报占用")
	}
	inst.Release()
	if InUse(dir) {
		t.Fatal("释放后不该再报占用")
	}
}

// 拒绝启动时还没有 Register 过，Release 不能把仍在运行的旧实例的登记抹掉：否则
// 连着启动两次就查不到它了。
func TestReleaseKeepsRegistrationItNeverWrote(t *testing.T) {
	dir := t.TempDir()
	old := []byte(`{"pid":123,"addr":"127.0.0.1:8090"}`)
	if err := os.WriteFile(Path(dir), old, 0o644); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		inst, err := Lock(dir)
		if err != nil {
			t.Fatalf("第 %d 次取锁失败：%v", attempt, err)
		}
		inst.Release()
		info, ok := readInfo(Path(dir))
		if !ok || info.PID != 123 {
			t.Fatalf("第 %d 次退出抹掉了旧实例的登记：%+v %v", attempt, info, ok)
		}
	}
}

// 交接被打断（改名之后、判断之前进程退出）：登记停在 .releasing 里，读取端仍要
// 找得到仍在运行的那个实例。
func TestReadFindsRegistrationLeftByInterruptedRelease(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	body := fmt.Sprintf(`{"pid":%d,"addr":"127.0.0.1:8090","started":%q}`,
		me, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(releasingPath(Path(dir)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, ok := Read(dir); !ok || info.PID != me {
		t.Fatalf("被打断的交接没有留下可发现的登记：%+v %v", info, ok)
	}
	// 新的登记写回来之后，以它为准。
	if err := os.WriteFile(Path(dir), []byte(`{"pid":456,"addr":"127.0.0.1:8091"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, ok := readInfo(Path(dir)); !ok || info.PID != 456 {
		t.Fatalf("有正式登记时不该回落到 .releasing：%+v %v", info, ok)
	}
}

// 判不出身份时不能当成「没有实例」：配置工具会因此退回离线写文件，改动会被仍在
// 运行的服务覆盖。
func TestReadStateReportsUnknownWithoutIdentity(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf(`{"pid":%d,"addr":"127.0.0.1:8090"}`, os.Getpid())
	if err := os.WriteFile(releasingPath(Path(dir)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	info, state := ReadState(dir)
	if state != StateUnknown || info.PID != os.Getpid() {
		t.Fatalf("state=%v info=%+v，想要 StateUnknown", state, info)
	}
	// Read 仍然报告「有登记」：不能写，但也不能当成没有实例。
	if got, ok := Read(dir); !ok || got.PID != os.Getpid() {
		t.Fatalf("Read 把 StateUnknown 当成了没有实例：%+v %v", got, ok)
	}
}

// PID 被复用的情形：登记里的 PID 现在确实活着，但它比这个进程还老，说明写登记的
// 那个进程早已退出，活着的是后来占了同一个号的新进程。
func TestReadRejectsRegistrationOlderThanProcess(t *testing.T) {
	if _, known := processStart(os.Getpid()); !known {
		t.Skip("这个平台查不到进程创建时间")
	}
	dir := t.TempDir()
	body := fmt.Sprintf(`{"pid":%d,"addr":"127.0.0.1:8090","started":%q}`,
		os.Getpid(), time.Now().Add(-24*time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(releasingPath(Path(dir)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, ok := Read(dir); ok {
		t.Fatalf("采纳了 PID 复用之后的旧登记：%+v", info)
	}
}

// 正式登记也要核对身份：写它的进程已经不在了，地址可能已经被另一份配置目录的
// 实例接手。以前只看「文件在不在」，探活成功就会连过去改别人的配置。
func TestReadIgnoresPrimaryRegistrationOfDeadProcess(t *testing.T) {
	dir := t.TempDir()
	body := fmt.Sprintf(`{"pid":%d,"addr":"127.0.0.1:8090","started":%q}`,
		1<<30, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(Path(dir), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, ok := Read(dir); ok {
		t.Fatalf("采纳了已退出进程留下的正式登记：%+v", info)
	}
}

// 正式登记里那个 PID 已经归了别人（创建时间晚于登记），同样不能采纳。
func TestReadIgnoresPrimaryRegistrationOlderThanProcess(t *testing.T) {
	if _, known := processStart(os.Getpid()); !known {
		t.Skip("这个平台查不到进程创建时间")
	}
	dir := t.TempDir()
	body := fmt.Sprintf(`{"pid":%d,"addr":"127.0.0.1:8090","started":%q}`,
		os.Getpid(), time.Now().Add(-24*time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(Path(dir), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, ok := Read(dir); ok {
		t.Fatalf("采纳了 PID 复用之后的正式登记：%+v", info)
	}
}

// 旧实例已经退出、交接留下的登记还在，而端口被另一份配置目录的服务复用：那个
// 地址即使应答，也不能当成这边的实例——否则 wen config 改的是别人的配置。
func TestReadIgnoresStaleReleasingRegistration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	// 1<<30 不可能是活着的 PID：这就是「旧实例已经退出」。
	body := fmt.Sprintf(`{"pid":%d,"addr":%q}`, 1<<30, strings.TrimPrefix(srv.URL, "http://"))
	if err := os.WriteFile(releasingPath(Path(dir)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if info, ok := Read(dir); ok {
		t.Fatalf("采纳了已退出实例的备用登记（地址属于另一实例）：%+v", info)
	}
}

// 新版退出时，旧版实例正好完成登记：不管它落在改名之前还是之后，那份登记都必须
// 活下来。
func TestReleaseDoesNotSwallowConcurrentRegistration(t *testing.T) {
	old := []byte(`{"pid":123,"addr":"127.0.0.1:8091"}`)
	for i := 0; i < 50; i++ {
		dir := t.TempDir()
		inst, err := Lock(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := inst.Register(Info{PID: 1, Addr: "127.0.0.1:8090"}); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			deadline := time.Now().Add(2 * time.Second)
			for {
				if err := os.WriteFile(Path(dir), old, 0o644); err == nil {
					return
				}
				if time.Now().After(deadline) {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		inst.Release()
		<-done
		if info, ok := readInfo(Path(dir)); !ok || info.PID != 123 {
			t.Fatalf("第 %d 次：旧实例的登记被删掉：%+v %v", i, info, ok)
		}
	}
}

// 旧版服务只写登记、不持运行权：新版取到锁之后、Register 之前，必须还能读到它的
// 地址，否则 startup 无法发现它仍在运行。
func TestLockKeepsExistingRegistrationUntilRegister(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte(`{"pid":123,"addr":"127.0.0.1:8090"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	inst, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Release()
	if info, ok := readInfo(Path(dir)); !ok || info.Addr != "127.0.0.1:8090" {
		t.Fatalf("取锁时覆盖了已有登记：%+v %v", info, ok)
	}
	if err := inst.Register(Info{PID: 4, Addr: "127.0.0.1:8091"}); err != nil {
		t.Fatal(err)
	}
	if info, _ := readInfo(Path(dir)); info.PID != 4 {
		t.Fatalf("登记没有写进去：%+v", info)
	}
}
