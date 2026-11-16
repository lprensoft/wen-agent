package memory

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"wen/internal/atomicfile"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// 本文件是语义召回的向量索引。词面召回（recall.go）不懂同义词：说「我最近睡不好」，
// 记忆里写的是「失眠、对咖啡因敏感」，两边一个词都对不上。向量补的就是这一块。
//
// 它是个**可重建的缓存**，不是第二份真相：记忆的正文只在 .md 文件里，索引里只有
// 「正文的哈希 + 向量」，哪天删掉它，下一轮对话会在后台悄悄重建，召回退回纯词面。
// 因此：
//   - 索引文件放在各记忆库目录里（.vectors.json），跟着库走，按可见域天然分开——
//     另一个域的向量不会出现在这个域的文件里，也就不会泄漏它的存在；
//   - 条目内容变了（哈希对不上）就当没有向量，等后台重算；
//   - 嵌入服务换了模型或维度（身份对不上）就整份作废重建，不同模型的向量不可比；
//   - 一切失败都静默降级：嵌入是默认关闭的增强，不该成为对话的故障点。
//
// 建索引放在后台，查询时只同步嵌入本轮这一句话。条目几百到几千条，内存里暴力算
// 余弦就够，不需要向量库。

const (
	vecIndexFile = ".vectors.json"
	vecVersion   = 1

	// embedTextRunes 是一条记忆送去嵌入的字数上限。标题、摘要与正文开头已经足够
	// 说明「这条讲什么」，更长的尾巴只会稀释向量，还白花 token。
	embedTextRunes = 1500
	// minSemanticRunes 以下的输入不做语义召回：「嗯」「好的」不值得为它发一次请求。
	// 比词面那条（minInputTokens）宽，因为语义召回恰恰要接住「睡不好」这种短句。
	minSemanticRunes = 3
	// queryEmbedTimeout 是同步嵌入本轮输入的时限。它在轮次开头的路径上，
	// 超过就放弃语义这一路，用纯词面——不能让一个慢服务拖住每一轮对话。
	queryEmbedTimeout = 4 * time.Second
	// syncTimeout 是一次后台建索引的总时限。
	syncTimeout = 3 * time.Minute
	// syncChunk 是后台建索引时每次落盘的条数：一批批存，中途失败也保住已有进展。
	syncChunk = 64
	// embedBackoff 是嵌入请求失败之后暂停使用的时长，避免服务宕机时每一轮都等一次超时。
	embedBackoff = 3 * time.Minute
)

// vecRecord 是一条记忆的向量，hash 是它对应文本的指纹。
type vecRecord struct {
	hash string
	vec  []float32
}

// vecIndex 是一个记忆库的向量索引，惰性从盘上装入。
type vecIndex struct {
	mu     sync.Mutex
	path   string
	loaded bool
	model  string // 这批向量的身份，见 plugin.EmbedResult.Model
	recs   map[string]vecRecord
}

// vecFile 是落盘格式。向量用小端 float32 的 base64：比 JSON 数组省三分之二，
// 而且解码不会引入浮点格式化的误差。
type vecFile struct {
	Version int                     `json:"version"`
	Model   string                  `json:"model"`
	Entries map[string]vecFileEntry `json:"entries"`
}

type vecFileEntry struct {
	Hash string `json:"h"`
	Vec  string `json:"v"`
}

func newVecIndex(dir string) *vecIndex {
	return &vecIndex{path: filepath.Join(dir, vecIndexFile), recs: map[string]vecRecord{}}
}

// loadLocked 读盘。任何问题（不存在、损坏、版本不对）都当成空索引：它是缓存，
// 坏了重建就是，不值得为它报错。
func (ix *vecIndex) loadLocked() {
	if ix.loaded {
		return
	}
	ix.loaded = true
	raw, err := os.ReadFile(ix.path)
	if err != nil {
		return
	}
	var f vecFile
	if json.Unmarshal(raw, &f) != nil || f.Version != vecVersion {
		return
	}
	recs := make(map[string]vecRecord, len(f.Entries))
	for slug, e := range f.Entries {
		v, ok := decodeVec(e.Vec)
		if !ok {
			continue
		}
		recs[slug] = vecRecord{hash: e.Hash, vec: v}
	}
	ix.model, ix.recs = f.Model, recs
}

// view 返回身份与 model 一致的向量；身份对不上时返回 nil。返回的是浅拷贝：
// 向量本身存入后不再修改，调用方可以放心在锁外读。
func (ix *vecIndex) view(model string) map[string]vecRecord {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.loadLocked()
	if ix.model != model || len(ix.recs) == 0 {
		return nil
	}
	return maps.Clone(ix.recs)
}

// stale 判断索引与这批条目对不上：缺向量、哈希变了、多出已删除条目，或身份不同。
// 只比较、不联网，每轮调用都便宜。want 是 slug → 文本哈希。
func (ix *vecIndex) stale(model string, want map[string]string) bool {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.loadLocked()
	if len(want) == 0 && len(ix.recs) == 0 {
		return false
	}
	if ix.model != model {
		return true
	}
	for slug, h := range want {
		if r, ok := ix.recs[slug]; !ok || r.hash != h {
			return true
		}
	}
	for slug := range ix.recs {
		if _, ok := want[slug]; !ok {
			return true
		}
	}
	return false
}

// apply 写入一批向量并丢掉 keep 里已不存在的条目，然后落盘。身份变了就先清空：
// 新旧向量不能混在一份索引里。
func (ix *vecIndex) apply(model string, upserts map[string]vecRecord, keep map[string]string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.loadLocked()
	if ix.model != model {
		ix.model, ix.recs = model, map[string]vecRecord{}
	}
	for slug := range ix.recs {
		if _, ok := keep[slug]; !ok {
			delete(ix.recs, slug)
		}
	}
	for slug, r := range upserts {
		ix.recs[slug] = r
	}
	return ix.saveLocked()
}

func (ix *vecIndex) saveLocked() error {
	f := vecFile{Version: vecVersion, Model: ix.model, Entries: make(map[string]vecFileEntry, len(ix.recs))}
	for slug, r := range ix.recs {
		f.Entries[slug] = vecFileEntry{Hash: r.hash, Vec: encodeVec(r.vec)}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ix.path), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(ix.path, raw, 0o644)
}

func encodeVec(v []float32) string {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func decodeVec(s string) ([]float32, bool) {
	buf, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(buf) == 0 || len(buf)%4 != 0 {
		return nil, false
	}
	v := make([]float32, len(buf)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[4*i:]))
	}
	return v, true
}

// dot 是两个单位向量的点积，即余弦相似度。维度不同返回 0：对不上的向量没有相似度可言。
func dot(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// semanticLen 是输入里非空白字符的个数。
func semanticLen(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// embedText 是一条记忆送去嵌入的文本：标题、摘要与正文的开头。
func embedText(e Entry) string {
	return truncateRunes(strings.TrimSpace(e.Name+"\n"+e.Description+"\n"+e.Content), embedTextRunes)
}

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// entryHashes 算出一批条目的文本指纹，键是 slug。
func entryHashes(entries []Entry) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[e.Slug] = textHash(embedText(e))
	}
	return out
}

// recallKey 是召回时标识一条记忆的键。同名记忆跨库去重之后，(域, slug) 仍是唯一的。
func recallKey(e Entry) string { return e.Domain + "\x00" + e.Slug }

// ---------- 插件侧：索引的持有、后台同步与查询 ----------

// vecIndexFor 返回某个记忆库目录的索引（惰性创建）。
func (p *Plugin) vecIndexFor(dir string) *vecIndex {
	p.vecMu.Lock()
	defer p.vecMu.Unlock()
	if p.vecs == nil {
		p.vecs = map[string]*vecIndex{}
	}
	ix, ok := p.vecs[dir]
	if !ok {
		ix = newVecIndex(dir)
		p.vecs[dir] = ix
	}
	return ix
}

// semanticOn 回答「这一轮能不能用语义召回」：有嵌入入口、本插件允许、召回没关，
// 且用户在设置页启用了嵌入服务。
func (s settings) semanticOn() (string, bool) {
	if s.embed == nil || s.embedModel == nil || !s.semantic || s.recallMax <= 0 {
		return "", false
	}
	model := s.embedModel()
	return model, model != ""
}

func (p *Plugin) embedBlocked() bool {
	p.embedMu.Lock()
	defer p.embedMu.Unlock()
	return time.Now().Before(p.embedBlockedUntil)
}

// blockEmbed 在嵌入请求失败后暂停使用一阵。返回 true 表示这是一次新的暂停
// （调用方据此只记一行日志，而不是每轮都记）。
func (p *Plugin) blockEmbed() bool {
	p.embedMu.Lock()
	defer p.embedMu.Unlock()
	fresh := !time.Now().Before(p.embedBlockedUntil)
	p.embedBlockedUntil = time.Now().Add(embedBackoff)
	return fresh
}

// noteEmbedErr 处理一次嵌入失败：未启用是常态，静默；其余暂停一阵并记一行日志。
func (p *Plugin) noteEmbedErr(lang string, err error) {
	if err == nil || errors.Is(err, plugin.ErrEmbedUnavailable) || errors.Is(err, context.Canceled) {
		return
	}
	if p.blockEmbed() {
		i18n.Logf(lang, "log.memory.embed_failed", "记忆语义召回：嵌入请求失败，%d 分钟内改用词面匹配：%v",
			int(embedBackoff/time.Minute), err)
	}
}

// kickIndex 检查各可读库的向量索引是否与记忆对得上，对不上就起一个后台任务补齐。
// 在每轮的 TurnPrompt 里调用，因此检查本身不联网、不阻塞——只比哈希。
// 这一个触发点同时覆盖了所有让记忆变化的路径（工具保存、提炼、淡忘、手工改文件）。
//
// 比对的是各库自己的完整清单，而不是本轮可见的那份：可见清单跨库按标题去过重，
// 被另一个域里同名记忆遮住的条目不在其中，拿它当「这个库有什么」，那些条目的
// 有效向量就会被当成已删除清掉，切回那个域时还要重新付费嵌入。
func (p *Plugin) kickIndex(ctx context.Context, s settings) {
	model, ok := s.semanticOn()
	if !ok || s.ctx == nil || p.embedBlocked() {
		return
	}
	for _, tag := range p.readDomains(ctx) {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		entries, err := store.List()
		if err != nil {
			continue
		}
		if !p.vecIndexFor(store.Dir()).stale(model, entryHashes(entries)) {
			continue
		}
		p.startSync(s, store)
	}
}

// startSync 起一个后台同步，同一个库同时只跑一个。登记在 turnMu 内完成，
// 与 Stop 的「先置 stopped 再 Wait」配合，不会漏等一个刚起来的 goroutine。
func (p *Plugin) startSync(s settings, store *Store) {
	dir := store.Dir()
	p.turnMu.Lock()
	if p.stopped || p.syncing[dir] {
		p.turnMu.Unlock()
		return
	}
	if p.syncing == nil {
		p.syncing = map[string]bool{}
	}
	p.syncing[dir] = true
	p.wg.Add(1)
	p.turnMu.Unlock()

	go func() {
		defer p.wg.Done()
		defer func() {
			p.turnMu.Lock()
			delete(p.syncing, dir)
			p.turnMu.Unlock()
		}()
		p.syncIndex(s, store)
	}()
}

// syncIndex 把一个库里缺失或过期的向量补齐，并丢掉已删除条目的。一批批落盘。
// 条目取自库本身而不是调用方传入的清单，理由见 kickIndex。
func (p *Plugin) syncIndex(s settings, store *Store) {
	model, ok := s.semanticOn()
	if !ok || s.ctx == nil {
		return
	}
	entries, err := store.List()
	if err != nil {
		return
	}
	ix := p.vecIndexFor(store.Dir())
	want := entryHashes(entries)
	have := ix.view(model)

	var need []Entry
	for _, e := range entries {
		if r, ok := have[e.Slug]; !ok || r.hash != want[e.Slug] {
			need = append(need, e)
		}
	}
	if len(need) == 0 {
		// 只有多出的记录要清掉（条目被删了）
		if err := ix.apply(model, nil, want); err != nil {
			i18n.Logf(s.lang, "log.memory.vec_save_failed", "记忆向量索引：写入失败：%v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, syncTimeout)
	defer cancel()
	done := 0
	for chunk := range slices.Chunk(need, syncChunk) {
		texts := make([]string, len(chunk))
		for i, e := range chunk {
			texts[i] = embedText(e)
		}
		res, err := s.embed(ctx, texts)
		if err != nil {
			p.noteEmbedErr(s.lang, err)
			return
		}
		if len(res.Vectors) != len(chunk) {
			return
		}
		upserts := make(map[string]vecRecord, len(chunk))
		for i, e := range chunk {
			upserts[e.Slug] = vecRecord{hash: want[e.Slug], vec: res.Vectors[i]}
		}
		if err := ix.apply(res.Model, upserts, want); err != nil {
			i18n.Logf(s.lang, "log.memory.vec_save_failed", "记忆向量索引：写入失败：%v", err)
			return
		}
		done += len(chunk)
	}
	i18n.Logf(s.lang, "log.memory.vec_indexed", "记忆向量索引：已为 %d 条记忆建立向量", done)
}

// semanticSims 嵌入本轮输入，与可读记忆的向量逐条算余弦相似度，键见 recallKey。
// 任何一步不成立都返回 nil——调用方据此退回纯词面，这一路从不报错。
func (p *Plugin) semanticSims(ctx context.Context, s settings, input string, entries []Entry) map[string]float32 {
	model, ok := s.semanticOn()
	if !ok || p.embedBlocked() || semanticLen(input) < minSemanticRunes {
		return nil
	}
	byDomain := map[string][]Entry{}
	vecs := map[string]map[string]vecRecord{}
	total := 0
	for _, e := range entries {
		byDomain[e.Domain] = append(byDomain[e.Domain], e)
	}
	for tag := range byDomain {
		store := p.storeFor(tag)
		if store == nil {
			continue
		}
		v := p.vecIndexFor(store.Dir()).view(model)
		vecs[tag] = v
		total += len(v)
	}
	if total == 0 {
		return nil // 一条向量都没有，嵌入这句话也没东西可比
	}

	qctx, cancel := context.WithTimeout(ctx, queryEmbedTimeout)
	defer cancel()
	res, err := s.embed(qctx, []string{input})
	if err != nil {
		// 本轮自己被取消不是服务的错
		if ctx.Err() == nil {
			p.noteEmbedErr(s.lang, err)
		}
		return nil
	}
	if len(res.Vectors) != 1 || res.Model != model {
		return nil // 配置在这一刻被改了：身份对不上的向量不能比
	}
	q := res.Vectors[0]

	sims := map[string]float32{}
	for tag, es := range byDomain {
		recs := vecs[tag]
		hashes := entryHashes(es)
		for _, e := range es {
			r, ok := recs[e.Slug]
			// 哈希对不上说明条目改过、向量是旧的：宁可不比，等后台重算
			if !ok || r.hash != hashes[e.Slug] {
				continue
			}
			sims[recallKey(e)] = dot(q, r.vec)
		}
	}
	return sims
}
