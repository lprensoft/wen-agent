package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"wen/internal/config"
	"wen/internal/modelcfg"
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

// 离线配置入口的脱敏往返：两家旧配置里只差大小写的提供商，改名第一家、密钥留空，
// 保存后两家的密钥仍各归各家。Ref 必须从 Snapshot 一路带进 toFile，否则服务端只能
// 按名字 EqualFold 配对，密钥会被抄到另一家。
func TestOfflineModelsRenameKeepsProviderKeys(t *testing.T) {
	base := t.TempDir()
	cfg, err := config.Load(filepath.Join(base, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw := `{
      "version": 1,
      "providers": [
        {"name":"Ζευς","type":"openai_compat","base_url":"https://a.example.com","api_key":"sk-a",
         "models":[{"id":"m"}]},
        {"name":"ΖΕΥΣ","type":"openai_compat","base_url":"https://b.example.com","api_key":"sk-b",
         "models":[{"id":"m"}]}
      ],
      "current": {"provider":"Ζευς","model":"m"}
    }`
	if err := os.WriteFile(filepath.Join(base, "models.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := newOfflineBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := b.loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Providers) != 2 {
		t.Fatalf("providers = %d", len(doc.Providers))
	}
	for _, p := range doc.Providers {
		if p.Ref == "" {
			t.Fatalf("离线视图也应携带 Ref，否则改名后认不回原条目: %+v", p)
		}
	}
	// 与 CLI 的编辑流程一致：只改条目，当前选择不手动同步，由 applyProviderEdit 带上。
	edited := modelcfg.Provider{
		Ref: doc.Providers[0].Ref, Name: "Zeus", Type: doc.Providers[0].Type, BaseURL: doc.Providers[0].BaseURL,
		Dialect: doc.Providers[0].Dialect, Protocol: doc.Providers[0].Protocol,
		PromptCache: doc.Providers[0].PromptCache, Models: doc.Providers[0].Models,
	}
	if err := b.saveModels(applyProviderEdit(doc.toFile(), doc.Providers[0], edited)); err != nil {
		t.Fatalf("改名保存: %v", err)
	}

	keys := map[string]string{}
	for _, p := range b.models.Snapshot().Providers {
		keys[p.Name] = p.APIKey
	}
	if keys["Zeus"] != "sk-a" || keys["ΖΕΥΣ"] != "sk-b" {
		t.Fatalf("改名保存后密钥应对应原来的条目: %+v", keys)
	}

	// 改名的是当前在用的那一家：保存后实际调用目标必须仍是它（同一份密钥、同一个
	// 地址），不能因为按旧名字找不到就落到只差大小写的另一家。
	r, err := b.models.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if r.ProviderName != "Zeus" || r.APIKey != "sk-a" || r.BaseURL != "https://a.example.com" {
		t.Fatalf("改名后当前模型被换掉了: %+v", r)
	}

	// 改的是不在当前选择上的那一家：当前选择不动。
	doc, err = b.loadModels()
	if err != nil {
		t.Fatal(err)
	}
	if doc.Providers[1].Name != "ΖΕΥΣ" {
		t.Fatalf("第二家应在原位: %+v", doc.Providers)
	}
	second := doc.Providers[1]
	edited = modelcfg.Provider{
		Ref: second.Ref, Name: "Hera", Type: second.Type, BaseURL: second.BaseURL,
		Dialect: second.Dialect, Protocol: second.Protocol, PromptCache: second.PromptCache,
		Models: second.Models,
	}
	if err := b.saveModels(applyProviderEdit(doc.toFile(), second, edited)); err != nil {
		t.Fatalf("改第二家的名字: %v", err)
	}
	if r, err := b.models.Resolve(""); err != nil || r.ProviderName != "Zeus" {
		t.Fatalf("改另一家的名字不该动当前模型: %+v, %v", r, err)
	}
}

func TestProviderEditKeepsFallbackTargets(t *testing.T) {
	models := []modelcfg.Model{{ID: "keep"}, {ID: "removed"}}
	for _, tc := range []struct {
		name, other string
		edit        int
		want        []modelcfg.Selection
	}{
		{"改名第二家保留第一家的备用项", "ΖΕΥΣ", 1, []modelcfg.Selection{
			{Provider: "Ζευς", Model: "removed"}, {Provider: "ΖΕΥΣ", Model: "keep"},
		}},
		{"改名第一家更新实际指向它的备用项", "ΖΕΥΣ", 0, []modelcfg.Selection{{Provider: "Zeus", Model: "keep"}}},
		{"唯一提供商的大小写别名跟着改名", "Hera", 0, []modelcfg.Selection{{Provider: "Zeus", Model: "keep"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := modelcfg.File{
				Providers: []modelcfg.Provider{
					{Name: "Ζευς", Ref: "first", Models: models},
					{Name: tc.other, Ref: "second", Models: models[0:1]},
				},
				Fallbacks: []modelcfg.Selection{
					{Provider: "Ζευς", Model: "removed"}, {Provider: "ΖΕΥΣ", Model: "keep"},
				},
			}
			old := file.Providers[tc.edit]
			edited := old
			edited.Name, edited.Models = "Zeus", models[0:1]
			got := applyProviderEdit(file, providerView{Name: old.Name, Ref: old.Ref}, edited)
			if !slices.Equal(got.Fallbacks, tc.want) {
				t.Fatalf("备用项 = %+v，期望 %+v", got.Fallbacks, tc.want)
			}
		})
	}
}

// /api/models 的往返：编辑模型时只改显示名，价格必须原样留在 models.json 里。
// Web 端 saveModel 已经改成以原模型为底，这一条盯住服务端这一侧不会把它丢掉。
func TestModelsSaveKeepsPrice(t *testing.T) {
	base := t.TempDir()
	cfg, err := config.Load(filepath.Join(base, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "models.json"), []byte(usageModelsDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := newOfflineBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}

	doc, err := b.loadModels()
	if err != nil {
		t.Fatal(err)
	}
	// 只改显示名，其余字段（含价格）原样提交
	for pi := range doc.Providers {
		for mi := range doc.Providers[pi].Models {
			m := &doc.Providers[pi].Models[mi]
			if m.ID == "mA" {
				m.Name = "改过的名字"
			}
		}
	}
	if err := b.saveModels(doc.toFile()); err != nil {
		t.Fatal(err)
	}

	doc, err = b.loadModels()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range doc.Providers {
		for _, m := range p.Models {
			if m.ID != "mA" {
				continue
			}
			if m.Name != "改过的名字" {
				t.Fatalf("显示名没有保存下来: %+v", m)
			}
			if m.Price == nil || m.Price.Currency != "CNY" || m.Price.Input != 1 {
				t.Fatalf("价格在保存后丢了: %+v", m.Price)
			}
			return
		}
	}
	t.Fatal("保存后找不到 mA")
}
