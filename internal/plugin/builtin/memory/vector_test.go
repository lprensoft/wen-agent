package memory

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wen/internal/plugin"
)

// ---------- 向量编码与索引文件 ----------

func TestVecEncodeRoundTrip(t *testing.T) {
	in := []float32{0, 1, -1, 0.123456, float32(math.Pi)}
	out, ok := decodeVec(encodeVec(in))
	if !ok || len(out) != len(in) {
		t.Fatalf("round trip 失败：%v %v", out, ok)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("[%d] %v != %v，base64 float32 应当无损", i, in[i], out[i])
		}
	}
	for _, bad := range []string{"", "!!!", "AAA="} { // 空、非法 base64、长度不是 4 的倍数
		if _, ok := decodeVec(bad); ok {
			t.Errorf("%q 不该解出向量", bad)
		}
	}
}

func TestVecIndexPersistsAndRejectsOtherModel(t *testing.T) {
	dir := t.TempDir()
	ix := newVecIndex(dir)
	err := ix.apply("m1", map[string]vecRecord{"a": {hash: "h", vec: []float32{1, 0}}}, map[string]string{"a": "h"})
	if err != nil {
		t.Fatal(err)
	}

	reloaded := newVecIndex(dir)
	if got := reloaded.view("m1"); got["a"].hash != "h" || len(got["a"].vec) != 2 {
		t.Fatalf("重载后 = %+v", got)
	}
	if reloaded.view("m2") != nil {
		t.Error("身份不同的向量不能拿来用")
	}
	if !reloaded.stale("m2", map[string]string{"a": "h"}) {
		t.Error("换了模型，索引应判为过期")
	}
}

func TestVecIndexStaleDetection(t *testing.T) {
	ix := newVecIndex(t.TempDir())
	if ix.stale("m", nil) {
		t.Error("没有条目也没有向量，不算过期")
	}
	_ = ix.apply("m", map[string]vecRecord{"a": {hash: "1", vec: []float32{1}}}, map[string]string{"a": "1"})
	cases := map[string]struct {
		want  map[string]string
		stale bool
	}{
		"一致":   {map[string]string{"a": "1"}, false},
		"哈希变了": {map[string]string{"a": "2"}, true},
		"多了条目": {map[string]string{"a": "1", "b": "1"}, true},
		"少了条目": {map[string]string{}, true},
	}
	for name, c := range cases {
		if got := ix.stale("m", c.want); got != c.stale {
			t.Errorf("%s：stale = %v, want %v", name, got, c.stale)
		}
	}
}

func TestVecIndexCorruptFileIsJustEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(newVecIndex(dir).path, []byte("{坏了"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix := newVecIndex(dir)
	if ix.view("m") != nil || !ix.stale("m", map[string]string{"a": "1"}) {
		t.Error("损坏的索引应当当作空索引，留给后台重建")
	}
}

// 索引文件放在记忆库目录里，不能被当成一条记忆。
func TestIndexFileIsNotAnEntry(t *testing.T) {
	p := newTestPlugin(t, nil)
	saveCat(t, p, plugin.Scope{})
	store := p.storeFor("")
	if err := newVecIndex(store.Dir()).apply("m", nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := store.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %d, err = %v", len(entries), err)
	}
}

// ---------- 融合与阈值 ----------

func TestPickRecallsSemanticOnlyHit(t *testing.T) {
	now := time.Now()
	sleep := Entry{Slug: "s", Name: "咖啡因与失眠", Content: "下午喝咖啡会失眠", Updated: now}
	cat := Entry{Slug: "c", Name: "小白那只猫", Content: "一只橘猫", Updated: now}
	sims := map[string]float32{recallKey(sleep): 0.9, recallKey(cat): 0.1}

	hits := pickRecalls("最近我总是睡不好觉怎么办", []Entry{sleep, cat}, 5, balancedLevel, sims)
	if len(hits) != 1 || hits[0].entry.Slug != "s" {
		t.Fatalf("词面对不上、语义相近的应被召回，其余不应：%+v", hits)
	}
}

func TestPickRecallsSemanticThresholdFollowsLevel(t *testing.T) {
	e := Entry{Slug: "s", Name: "咖啡因与失眠", Updated: time.Now()}
	sims := map[string]float32{recallKey(e): 0.5}
	input := "最近我总是睡不好觉怎么办"
	for level, want := range map[string]int{levelConservative: 0, levelBalanced: 1, levelEager: 1} {
		if got := len(pickRecalls(input, []Entry{e}, 5, recallLevels[level], sims)); got != want {
			t.Errorf("%s：0.5 的相似度召回 %d 条，want %d", level, got, want)
		}
	}
	sims[recallKey(e)] = 0.4
	for level, want := range map[string]int{levelConservative: 0, levelBalanced: 0, levelEager: 1} {
		if got := len(pickRecalls(input, []Entry{e}, 5, recallLevels[level], sims)); got != want {
			t.Errorf("%s：0.4 的相似度召回 %d 条，want %d", level, got, want)
		}
	}
}

func TestPickRecallsShortInputStillUsesSemantic(t *testing.T) {
	e := Entry{Slug: "s", Name: "咖啡因与失眠", Updated: time.Now()}
	sims := map[string]float32{recallKey(e): 0.9}
	// 「睡不好」切不出四个词面 token，纯词面一定放弃；语义那一路正是为这种短句准备的
	if hits := pickRecalls("睡不好", []Entry{e}, 5, balancedLevel, sims); len(hits) != 1 {
		t.Errorf("短句应能靠语义召回，实际 %d 条", len(hits))
	}
	if hits := pickRecalls("睡不好", []Entry{e}, 5, balancedLevel, nil); len(hits) != 0 {
		t.Errorf("没有语义结果时短句仍不该召回，实际 %d 条", len(hits))
	}
}

func TestPickRecallsFusionRanksBothRoutesFirst(t *testing.T) {
	now := time.Now()
	both := Entry{Slug: "both", Name: "小白那只猫", Description: "家里的橘猫", Content: "一只橘猫。", Updated: now}
	semOnly := Entry{Slug: "sem", Name: "毛孩子", Content: "家里的宠物", Updated: now}
	lexOnly := Entry{Slug: "lex", Name: "小白那只猫的旧事", Description: "家里的猫", Content: "一只橘猫。", Updated: now}
	sims := map[string]float32{recallKey(both): 0.6, recallKey(semOnly): 0.9, recallKey(lexOnly): 0.1}

	hits := pickRecalls("你还记得小白那只猫现在怎么样了吗", []Entry{lexOnly, semOnly, both}, 5, balancedLevel, sims)
	if len(hits) != 3 {
		t.Fatalf("三条都该过线，实际 %d 条", len(hits))
	}
	if hits[0].entry.Slug != "both" {
		t.Errorf("两路都上榜的应排第一，实际第一条是 %q", hits[0].entry.Slug)
	}
}

func TestPickRecallsWithoutSimsKeepsLexicalOrder(t *testing.T) {
	now := time.Now()
	short := entryAt("小白那只猫", "家里的猫", "一只橘猫。", now)
	long := entryAt("小白那只猫的一年", "家里的猫",
		strings.Repeat("它早上会跳到床头叫人起床，中午趴在窗台晒太阳，晚上钻进被窝。", 12), now)
	hits := pickRecalls("你还记得小白那只猫现在怎么样了吗", []Entry{long, short}, 5, balancedLevel, nil)
	if len(hits) != 2 || hits[0].entry.Name != short.Name {
		t.Fatalf("没有语义结果时应与纯词面一致：%+v", hits)
	}
}

func TestLevelsAreOrderedAndUnknownFallsBack(t *testing.T) {
	c, b, e := recallLevels[levelConservative], recallLevels[levelBalanced], recallLevels[levelEager]
	if !(c.minScore > b.minScore && b.minScore > e.minScore) ||
		!(c.contentOnly > b.contentOnly && b.contentOnly > e.contentOnly) ||
		!(c.semantic > b.semantic && b.semantic > e.semantic) {
		t.Errorf("三档的词面与语义阈值都应当由严到松：%+v %+v %+v", c, b, e)
	}
	if b.minScore != 4 || b.contentOnly != 4 {
		t.Errorf("适中档的词面阈值必须等于引入分档前的默认值（4/4），否则升级会改变没配过的人的召回：%+v", b)
	}
	if levelFor("乱写的") != b || levelFor("") != b {
		t.Error("不认识的档位名应回落到适中")
	}
}

// ---------- 插件：同步、查询与降级 ----------

// fakeEmbedder 把含「失眠/睡不好/睡眠」的文本投到一个方向、含「猫」的投到另一个方向，
// 其余落在一个小分量上，这样两段说法不同但意思相近的话相似度接近 1，无关的接近 0。
type fakeEmbedder struct {
	mu    sync.Mutex
	model string
	err   error
	calls atomic.Int32
	seen  []string
}

func (f *fakeEmbedder) setModel(m string) { f.mu.Lock(); f.model = m; f.mu.Unlock() }
func (f *fakeEmbedder) setErr(err error)  { f.mu.Lock(); f.err = err; f.mu.Unlock() }
func (f *fakeEmbedder) currentModel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.model
}

func (f *fakeEmbedder) embed(ctx context.Context, texts []string) (plugin.EmbedResult, error) {
	f.calls.Add(1)
	f.mu.Lock()
	model, err := f.model, f.err
	f.seen = append(f.seen, texts...)
	f.mu.Unlock()
	if err != nil {
		return plugin.EmbedResult{}, err
	}
	vecs := make([][]float32, len(texts))
	for i, t := range texts {
		sleep := strings.Count(t, "失眠") + strings.Count(t, "睡不好") + strings.Count(t, "睡眠")
		cat := strings.Count(t, "猫")
		v := []float32{float32(sleep), float32(cat), 0.01}
		var n float64
		for _, x := range v {
			n += float64(x * x)
		}
		for j := range v {
			v[j] /= float32(math.Sqrt(n))
		}
		vecs[i] = v
	}
	return plugin.EmbedResult{Model: model, Vectors: vecs}, nil
}

func (f *fakeEmbedder) sawText(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.seen {
		if strings.Contains(t, sub) {
			return true
		}
	}
	return false
}

func newSemanticPlugin(t *testing.T, f *fakeEmbedder, cfg map[string]any) *Plugin {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["turn_extract"] = false
	p := New()
	ictx := plugin.InitContext{StateDir: t.TempDir(), Embed: f.embed, EmbedModel: f.currentModel}
	if err := p.Init(ictx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop) // 等后台同步收尾，否则 Windows 上临时目录删不掉
	return p
}

func saveSleep(t *testing.T, p *Plugin, scope plugin.Scope) {
	t.Helper()
	_, err := p.storeFor(scope.Write).Save(Entry{
		Name: "咖啡因与失眠", Description: "下午喝咖啡会失眠", Type: "事实",
		Content: "对咖啡因敏感，下午四点以后不能喝咖啡，否则晚上失眠。",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
}

const sleepQuestion = "最近我总是睡不好觉怎么办"

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func TestSemanticRecallFindsWhatLexicalCannot(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, plugin.Scope{})

	// 第一轮：索引还没建，只能纯词面，什么也想不起来；同时触发后台建索引
	if got := turnPrompt(t, p, context.Background(), sleepQuestion); strings.Contains(got, recallHeaderZH) {
		t.Fatalf("索引还没建好时不该靠语义召回：\n%s", got)
	}
	store := p.storeFor("")
	waitUntil(t, "后台建好向量索引", func() bool {
		return p.vecIndexFor(store.Dir()).view("fake|m|0") != nil
	})

	got := turnPrompt(t, p, context.Background(), sleepQuestion)
	if !strings.Contains(got, recallHeaderZH) || !strings.Contains(got, "对咖啡因敏感") {
		t.Fatalf("索引建好后，「睡不好」应想起「失眠」那条：\n%s", got)
	}
}

func TestSemanticRecallOffWhenEmbeddingNotEnabled(t *testing.T) {
	f := &fakeEmbedder{model: ""} // 身份为空 = 用户没有启用嵌入服务
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, plugin.Scope{})

	for range 3 {
		if got := turnPrompt(t, p, context.Background(), sleepQuestion); strings.Contains(got, recallHeaderZH) {
			t.Fatalf("没启用嵌入服务不该召回：\n%s", got)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("没启用嵌入服务时一次请求都不该发，实际 %d 次", n)
	}
	if line := p.StatusLines("")[0]; strings.Contains(line, "语义召回") {
		t.Errorf("没启用时状态行不该宣称语义召回：%s", line)
	}
}

func TestSemanticRecallPluginSwitchKeepsMemoryLocal(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, map[string]any{"recall_semantic": false})
	saveSleep(t, p, plugin.Scope{})

	for range 3 {
		turnPrompt(t, p, context.Background(), sleepQuestion)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("关掉本插件的语义开关后，记忆正文与用户输入都不该发出去，实际请求 %d 次", n)
	}
}

func TestSemanticRecallSkippedWhenRecallDisabled(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, map[string]any{"recall_max": 0})
	saveSleep(t, p, plugin.Scope{})
	turnPrompt(t, p, context.Background(), sleepQuestion)
	if n := f.calls.Load(); n != 0 {
		t.Errorf("召回关闭时不需要向量，实际请求 %d 次", n)
	}
}

func TestSemanticRecallIgnoresEphemeralInputButStillIndexes(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, plugin.Scope{})
	store := p.storeFor("")

	ctx := plugin.WithEphemeralInput(context.Background())
	turnPrompt(t, p, ctx, sleepQuestion)
	waitUntil(t, "后台建好向量索引", func() bool { return p.vecIndexFor(store.Dir()).view("fake|m|0") != nil })
	if f.sawText(sleepQuestion) {
		t.Error("机器注入的一次性输入不该被拿去嵌入")
	}
}

func TestSemanticFailureFallsBackToLexicalAndBacksOff(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0", err: errors.New("503")}
	p := newSemanticPlugin(t, f, nil)
	saveCat(t, p, plugin.Scope{})

	// 嵌入服务挂了，词面召回照常工作
	got := turnPrompt(t, p, context.Background(), catQuestion)
	if !strings.Contains(got, recallHeaderZH) {
		t.Fatalf("嵌入失败时应退回词面召回：\n%s", got)
	}
	waitUntil(t, "后台同步尝试过一次", func() bool { return f.calls.Load() >= 1 })
	waitUntil(t, "失败后进入暂停", p.embedBlocked)
	before := f.calls.Load()
	for range 3 {
		turnPrompt(t, p, context.Background(), catQuestion)
	}
	if after := f.calls.Load(); after != before {
		t.Errorf("暂停期内不该再发请求：%d → %d", before, after)
	}
}

func TestIndexRebuildsWhenModelChanges(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m1|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, plugin.Scope{})
	store := p.storeFor("")
	ix := p.vecIndexFor(store.Dir())

	turnPrompt(t, p, context.Background(), sleepQuestion)
	waitUntil(t, "按旧模型建好索引", func() bool { return ix.view("fake|m1|0") != nil })

	f.setModel("fake|m2|0")
	// 新模型下旧向量不可比：召回退回词面，同时后台按新模型重建
	if got := turnPrompt(t, p, context.Background(), sleepQuestion); strings.Contains(got, recallHeaderZH) {
		t.Fatalf("换了模型，旧向量不能再用：\n%s", got)
	}
	waitUntil(t, "按新模型重建索引", func() bool { return ix.view("fake|m2|0") != nil })
	if ix.view("fake|m1|0") != nil {
		t.Error("旧模型的向量应当整体作废")
	}
}

func TestIndexFollowsEditsAndDeletes(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, plugin.Scope{})
	saveCat(t, p, plugin.Scope{})
	store := p.storeFor("")
	ix := p.vecIndexFor(store.Dir())
	sync := func() {
		t.Helper()
		p.syncIndex(p.snapshot(), store)
	}

	sync()
	if got := ix.view("fake|m|0"); len(got) != 2 {
		t.Fatalf("应为两条记忆建好向量，实际 %d 条", len(got))
	}

	calls := f.calls.Load()
	sync()
	if f.calls.Load() != calls {
		t.Error("没有变化时不该再发请求")
	}

	if _, err := store.Delete("小白那只猫"); err != nil {
		t.Fatal(err)
	}
	sync()
	if got := ix.view("fake|m|0"); len(got) != 1 {
		t.Errorf("删掉的记忆不该留在索引里，实际 %d 条", len(got))
	}
	if f.calls.Load() != calls {
		t.Error("只是删除不该触发重新嵌入")
	}

	if _, err := store.Save(Entry{Name: "咖啡因与失眠", Description: "下午喝咖啡会失眠", Type: "事实",
		Content: "改过了：现在连上午也不能喝咖啡。"}, true); err != nil {
		t.Fatal(err)
	}
	sync()
	if f.calls.Load() != calls+1 {
		t.Error("正文改了应当重新嵌入那一条")
	}
}

// 记忆被改过之后、后台还没来得及重算之前，旧向量描述的是旧正文，不能拿来召回。
func TestStaleVectorIsNotUsedForRecall(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, plugin.Scope{})
	store := p.storeFor("")
	entries, _ := p.visibleEntries(context.Background())
	p.syncIndex(p.snapshot(), store)

	s := p.snapshot()
	if sims := p.semanticSims(context.Background(), s, sleepQuestion, entries); len(sims) != 1 {
		t.Fatalf("新鲜的向量应当可用：%v", sims)
	}
	if _, err := store.Save(Entry{Name: "咖啡因与失眠", Description: "下午喝咖啡会失眠", Type: "事实",
		Content: "完全不同的正文。"}, true); err != nil {
		t.Fatal(err)
	}
	changed, _ := p.visibleEntries(context.Background())
	if sims := p.semanticSims(context.Background(), s, sleepQuestion, changed); len(sims) != 0 {
		t.Errorf("正文改了而向量是旧的，不该参与召回：%v", sims)
	}
}

func TestSemanticSkipsShortInputWithoutRequest(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, plugin.Scope{})
	store := p.storeFor("")
	entries, _ := p.visibleEntries(context.Background())
	p.syncIndex(p.snapshot(), store)

	before := f.calls.Load()
	if sims := p.semanticSims(context.Background(), p.snapshot(), "嗯", entries); sims != nil {
		t.Errorf("「嗯」不该算语义：%v", sims)
	}
	if f.calls.Load() != before {
		t.Error("太短的输入不值得发一次嵌入请求")
	}
}

// 可见域：向量索引按库分开存，另一个域的记忆不会经语义这一路露出来。
func TestSemanticRecallRespectsScope(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, innerScope) // 写进 inner 库

	innerStore := p.storeFor("inner")
	p.syncIndex(p.snapshot(), innerStore)
	if p.vecIndexFor(innerStore.Dir()).view("fake|m|0") == nil {
		t.Fatal("inner 库的索引应已建好")
	}

	if got := turnPrompt(t, p, scoped(outerScope), sleepQuestion); strings.Contains(got, "咖啡因") {
		t.Errorf("outer 域不该经语义召回看到 inner 域的记忆：\n%s", got)
	}
	if got := turnPrompt(t, p, scoped(innerScope), sleepQuestion); !strings.Contains(got, "对咖啡因敏感") {
		t.Errorf("inner 域应能召回自己的记忆：\n%s", got)
	}
}

func TestStatusLineMentionsSemanticRecallWhenOn(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	if line := p.StatusLines("")[0]; !strings.Contains(line, "语义召回") {
		t.Errorf("启用后状态行应提到语义召回：%s", line)
	}
}

// 跨域同名的记忆：可见清单里只留靠前的那条，被遮住的那条仍在它自己的库里，
// 它的有效向量不能因为「本轮看不见」就被当成已删除清掉。
func TestShadowedEntryKeepsItsVector(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, outerScope) // outer 库里有一条
	saveSleep(t, p, innerScope) // inner 库里同名，进 inner 域时遮住它

	outerStore, innerStore := p.storeFor("outer"), p.storeFor("inner")
	p.syncIndex(p.snapshot(), outerStore)
	p.syncIndex(p.snapshot(), innerStore)
	calls := f.calls.Load()

	turnPrompt(t, p, scoped(innerScope), sleepQuestion)
	p.wg.Wait() // 触发的后台同步（如果有）跑完

	if got := p.vecIndexFor(outerStore.Dir()).view("fake|m|0"); len(got) != 1 {
		t.Fatalf("被遮住的条目在 outer 库里仍存在，向量不该被清掉，实际 %d 条", len(got))
	}
	entries, _ := outerStore.List()
	if p.vecIndexFor(outerStore.Dir()).stale("fake|m|0", entryHashes(entries)) {
		t.Error("outer 库的索引应与它自己的条目一致")
	}
	// 之后的轮次里这条也不该再引发任何重新嵌入（只有查询本轮输入的那一次请求）
	if extra := f.calls.Load() - calls; extra > 1 {
		t.Errorf("没有任何变化，却多发了 %d 次嵌入请求", extra)
	}
}

// 「睡不好」只有三个字：词面凑不够四个 token，语义这一路正是为这种短句准备的，
// 必须从真正的召回入口走一遍，而不是直接喂相似度。
func TestThreeCharInputRecallsSemanticallyThroughTurnPrompt(t *testing.T) {
	f := &fakeEmbedder{model: "fake|m|0"}
	p := newSemanticPlugin(t, f, nil)
	saveSleep(t, p, plugin.Scope{})
	p.syncIndex(p.snapshot(), p.storeFor(""))

	if got := turnPrompt(t, p, context.Background(), "睡不好"); !strings.Contains(got, "对咖啡因敏感") {
		t.Fatalf("三个字的「睡不好」应能想起「失眠」那条：\n%s", got)
	}
	before := f.calls.Load()
	for _, short := range []string{"嗯", "好的", "  好  "} {
		turnPrompt(t, p, context.Background(), short)
	}
	if f.calls.Load() != before {
		t.Error("一两个字的寒暄不值得发嵌入请求")
	}
}
