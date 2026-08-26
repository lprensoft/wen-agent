package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

const (
	defaultMaxIndexEntries = 200
	defaultMaxIndexBytes   = 16 * 1024
	defaultMaxEntryBytes   = 8 * 1024
	defaultAutoExtract     = true
	defaultMaxExtract      = 5
	defaultTurnExtract     = true
	defaultTurnEvery       = 10
	defaultDecay           = false
	defaultBlurDays        = 30
	defaultForgetDays      = 90
	// maxExtractBytes 限制送去提炼的历史长度，避免一次压缩带来过高的调用成本。
	maxExtractBytes = 24 * 1024
	// turnExtractBytes 是定期提炼的历史上限。比压缩那次小一档：一个窗口本来
	// 就只有十来轮对话，装不满也没必要为此付更多 token。
	turnExtractBytes = 12 * 1024
	// minWindowBytes 以下的窗口不值得为它调一次模型（十轮「嗯」「好的」也会到点）。
	// 不清空缓冲，并进下一个窗口继续攒。
	minWindowBytes = 240
	// idleFlushGap：与上一轮相隔这么久再开口，视作上一段话题已经结束，
	// 先把攒着的窗口提炼掉再开新窗口。比「轮数够了」更贴近记忆该落盘的时机，
	// 而且不需要任何定时器——间隔从两轮的时间戳就能算出来。
	idleFlushGap = 30 * time.Minute
	// stopWait 是 Stop 等待在途提炼收尾的上限，超时就不再等（ctx 已经取消）。
	stopWait = 5 * time.Second
)

// 记忆库的存放范围。配置键沿用 scope，但代码里叫 library——「可见域」这个核心概念
// 也叫 scope，同名会让这段逻辑难以阅读。
const (
	libraryGlobal  = "global"  // 全局单库，所有工作目录共享
	libraryProject = "project" // 按工作目录分库
	defaultLibrary = libraryGlobal
)

// memoriesDir 按存放范围决定基准记忆目录。按可见域分出的库是它的同级目录，
// 见 plugin.DomainDir。
func memoriesDir(ictx plugin.InitContext, library string) (string, error) {
	if library == libraryProject {
		if ictx.Workdir == "" {
			return "", fmt.Errorf("%s", prompt.T(ictx.Lang, "prompt.memory.no_workdir",
				"按项目分库需要工作目录，当前未设置（可改用全局记忆库）"))
		}
		return filepath.Join(ictx.Workdir, ".wen", "memories"), nil
	}
	if ictx.StateDir == "" {
		return "", fmt.Errorf("%s", prompt.T(ictx.Lang, "prompt.memory.no_state_dir",
			"没有可用的持久化目录，无法保存记忆"))
	}
	return filepath.Join(ictx.StateDir, "memories"), nil
}

// Plugin 是 memory 系统插件：注入记忆索引，并提供读写记忆的工具。
type Plugin struct {
	mu sync.RWMutex

	lang            string // 进程语言，注入片段与工具文本据此取用
	libBase         string // 基准记忆库目录（= 共享域）
	store           *Store // 基准库，等价于 storeFor("")
	library         string
	complete        plugin.CompleteFunc
	dayFacts        plugin.DayFactsFunc
	notice          plugin.NoticeFunc
	maxIndexEntries int
	maxIndexBytes   int
	maxEntryBytes   int
	autoExtract     bool
	maxExtract      int
	turnExtract     bool
	turnEvery       int
	decay           bool
	blurDays        int
	forgetDays      int
	timeline        bool
	timelineDays    int
	// 自动召回的参数，见 recall.go。
	recallMax        int
	recallEntryRunes int
	recallMaxBytes   int
	recallMinScore   int

	// 后台提炼与清扫的运行环境。ctx 由 Init 建、Stop 取消——广播进来的 ctx
	// 在轮次结束时就被取消了，拿它去跑后台调用必然半路夭折。
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// 按可见域分出的库，惰性创建。单独一把锁：Store 自带目录指纹缓存，
	// 每次重建会白丢缓存，而这张表的生命周期与配置无关。
	storesMu sync.Mutex
	stores   map[string]*Store

	// 定期提炼的状态。单独一把锁，且不随 Init 重置：攒着的是对话内容，
	// 不是配置，改一次配置就把它清空等于白丢一个窗口。
	turnMu    sync.Mutex
	windowDir string // 窗口缓冲的落盘位置；为空表示无处可存，退化成纯内存缓冲
	loaded    bool   // 缓冲已从盘上装回，Init 重入时不再重复装（内存里的更新）
	windowSeq uint64 // 每次落盘递增，由 turnMu 保护

	// 写盘串行化。每轮对话都会起一个写盘 goroutine，它们完成的顺序不保证——
	// 不排队的话，晚到的旧快照会把新进展整个盖掉（表现为重启后只剩一轮）。
	saveMu     sync.Mutex
	savedSeq   uint64
	windows    map[windowKey]*window
	extracting map[windowKey]bool
	lastSweep  time.Time
	sweeping   bool
	// 时间线的日切状态（由 turnMu 保护）与文件锁。
	lastTimeline time.Time
	timelining   bool
	timelineMu   sync.Mutex
	// stopped 让 wg.Add 与 wg.Wait 有明确的先后：两者都在 turnMu 内决定，
	// 停止之后不再有新的 goroutine 被登记，Wait 也就不会漏等一个刚起来的。
	stopped bool
}

func New() *Plugin {
	return &Plugin{
		library:          defaultLibrary,
		maxIndexEntries:  defaultMaxIndexEntries,
		maxIndexBytes:    defaultMaxIndexBytes,
		maxEntryBytes:    defaultMaxEntryBytes,
		autoExtract:      defaultAutoExtract,
		maxExtract:       defaultMaxExtract,
		turnExtract:      defaultTurnExtract,
		turnEvery:        defaultTurnEvery,
		decay:            defaultDecay,
		blurDays:         defaultBlurDays,
		forgetDays:       defaultForgetDays,
		timeline:         defaultTimeline,
		timelineDays:     defaultTimelineDays,
		recallMax:        defaultRecallMax,
		recallEntryRunes: defaultRecallEntryRunes,
		recallMaxBytes:   defaultRecallMaxBytes,
		recallMinScore:   defaultRecallMinScore,
		windows:          map[windowKey]*window{},
		extracting:       map[windowKey]bool{},
	}
}

func (p *Plugin) Name() string { return "memory" }

func (p *Plugin) Category() string { return plugin.CategoryMemory }
func (p *Plugin) Description() string {
	return "跨会话的长期记忆，索引随对话注入，可按需读取全文"
}

func (p *Plugin) ConfigFields() []plugin.ConfigField {
	return []plugin.ConfigField{
		{
			Key:   "scope",
			Label: "记忆库范围",
			Type:  plugin.FieldSelect,
			Description: "全局：所有工作目录共享一份，存于配置目录下；" +
				"按项目：存于工作目录的 .wen/memories/，不同项目互不可见（建议加入该项目的 .gitignore）。" +
				"切换后原位置的记忆不会自动迁移。",
			Default: defaultLibrary,
			Options: []plugin.ConfigOption{
				{Value: libraryGlobal, Label: "全局"},
				{Value: libraryProject, Label: "按项目（工作目录）"},
			},
		},
		{
			Key:         "max_index_entries",
			Label:       "索引最多列出条数",
			Type:        plugin.FieldInt,
			Description: "超出后按最近更新保留，其余模型仍可自行查看，不会丢失。",
			Default:     defaultMaxIndexEntries,
			Min:         plugin.IntPtr(0),
			Max:         plugin.IntPtr(2000),
		},
		{
			Key:   "max_index_bytes",
			Label: "索引最大字节数",
			Type:  plugin.FieldInt,
			Description: "索引会随每一轮对话重复发送，因此需要上限。" +
				"超出时先省略各条的摘要只保留标题，仍超出才截断。",
			Default: defaultMaxIndexBytes,
			Min:     plugin.IntPtr(0),
			Max:     plugin.IntPtr(256 * 1024),
		},
		{
			Key:         "max_entry_bytes",
			Label:       "单条记忆最大返回字节数",
			Type:        plugin.FieldInt,
			Description: "读取记忆全文时超出部分截断。",
			Default:     defaultMaxEntryBytes,
			Min:         plugin.IntPtr(1024),
			Max:         plugin.IntPtr(1024 * 1024),
		},
		{
			Key:   "recall_max",
			Label: "自动想起的条数",
			Type:  plugin.FieldInt,
			Description: "每轮按你这句话里的词，从记忆里挑出最相关的几条，把正文直接带给角色——" +
				"像聊到某件事自然想起来，不用它自己去翻。纯词面匹配，不多花模型调用。0 表示关闭。",
			Default: defaultRecallMax,
			Min:     plugin.IntPtr(0),
			Max:     plugin.IntPtr(5),
		},
		{
			Key:         "recall_entry_runes",
			Label:       "每条想起的正文字数",
			Type:        plugin.FieldInt,
			Description: "自动想起的记忆正文超过这个字数时截断，后文仍可按标题读全文。",
			Default:     defaultRecallEntryRunes,
			Min:         plugin.IntPtr(100),
			Max:         plugin.IntPtr(2000),
		},
		{
			Key:   "recall_max_bytes",
			Label: "想起来的事总字节数",
			Type:  plugin.FieldInt,
			Description: "一轮里自动想起的内容合计不超过这个字节数，装不下的条目先让。" +
				"这段内容每轮重发且不走缓存，别开太大。",
			Default: defaultRecallMaxBytes,
			Min:     plugin.IntPtr(512),
			Max:     plugin.IntPtr(8 * 1024),
		},
		{
			Key:   "recall_min_score",
			Label: "想起来的最低相关度",
			Type:  plugin.FieldInt,
			Description: "一条记忆至少要和这句话有多相关才会被想起：标题或摘要里对上一个词算 3 分，" +
				"正文里对上一个词算 1 分；标题与摘要一个词都没对上时，正文要对上 4 个词才算。调低更容易想起、也更容易想歪。",
			Default: defaultRecallMinScore,
			Min:     plugin.IntPtr(1),
			Max:     plugin.IntPtr(20),
		},
		{
			Key:   "auto_extract",
			Label: "压缩时自动提炼记忆",
			Type:  plugin.FieldBool,
			Description: "会话历史被压缩摘要替换之前，用一次独立的模型调用从其中提炼值得长期保留的结论。" +
				"关闭后只有角色自己想起来才会保存记忆。每次压缩会因此多一次模型调用。",
			Default: defaultAutoExtract,
		},
		{
			Key:         "max_extract",
			Label:       "单次最多提炼条数",
			Type:        plugin.FieldInt,
			Description: "超出的部分丢弃，避免一次提炼灌入过多低价值记忆。",
			Default:     defaultMaxExtract,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(50),
		},
		{
			Key:   "turn_extract",
			Label: "对话中定期提炼记忆",
			Type:  plugin.FieldBool,
			Description: "每隔若干轮对话，用一次独立的模型调用从这段对话中提炼值得长期保留的结论，" +
				"顺带修订与之矛盾的旧记忆。不必等到压缩，大部分对话本来也走不到那一步。" +
				"每个提炼窗口会因此多一次模型调用。",
			Default: defaultTurnExtract,
		},
		{
			Key:   "turn_extract_every",
			Label: "提炼间隔（轮）",
			Type:  plugin.FieldInt,
			Description: "每多少轮真人对话提炼一次。机器自发的轮次（心跳、定时任务）不计入。" +
				"距上次对话超过半小时再开口时，会先把攒着的部分提炼掉。",
			Default: defaultTurnEvery,
			Min:     plugin.IntPtr(5),
			Max:     plugin.IntPtr(100),
		},
		{
			Key:   "timeline",
			Label: "按日时间线",
			Type:  plugin.FieldBool,
			Description: "把每天的对话在次日收束成一行「日期 | 关键事件」，最近几天随对话注入，" +
				"回答「我们最近经历了什么」。每个有对话的自然日多一次模型调用。",
			Default: defaultTimeline,
		},
		{
			Key:         "timeline_days",
			Label:       "时间线注入条数",
			Type:        plugin.FieldInt,
			Description: "随对话注入最近多少条时间线记录，更早的保留在库里不注入。",
			Default:     defaultTimelineDays,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(30),
		},
		{
			Key:   "decay",
			Label: "记忆逐步淡忘",
			Type:  plugin.FieldBool,
			Description: "让标记为会淡忘的记忆随时间失效：久未提及先塌缩成一句要点，" +
				"更久之后移出记忆库（移入库内的 forgotten 目录，不会真正删除）。" +
				"是否会淡忘由保存时逐条决定，未标记的记忆永久保留，不受此开关影响。",
			Default: defaultDecay,
		},
		{
			Key:         "decay_blur_days",
			Label:       "细节淡忘天数",
			Type:        plugin.FieldInt,
			Description: "一条会淡忘的记忆多少天没有被提及后，细节丢失，只剩一句要点。",
			Default:     defaultBlurDays,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(3650),
		},
		{
			Key:         "decay_forget_days",
			Label:       "移出记忆库天数",
			Type:        plugin.FieldInt,
			Description: "一条会淡忘的记忆多少天没有被提及后移出记忆库。需大于细节淡忘天数。",
			Default:     defaultForgetDays,
			Min:         plugin.IntPtr(1),
			Max:         plugin.IntPtr(3650),
		},
	}
}

// Init 需要一个可写目录；没有则拒绝启用，避免把记忆散落到进程当前目录。
func (p *Plugin) Init(ictx plugin.InitContext, cfg map[string]any) error {
	library := plugin.CfgString(cfg, "scope", defaultLibrary)
	dir, err := memoriesDir(ictx, library)
	if err != nil {
		return err
	}
	blurDays := plugin.CfgInt(cfg, "decay_blur_days", defaultBlurDays)
	forgetDays := plugin.CfgInt(cfg, "decay_forget_days", defaultForgetDays)
	// 跨字段的约束 NormalizeConfig 表达不了（它只认单字段的 Min/Max），在这里挡一次。
	// 先于任何副作用检查：Init 失败等于拒绝这次配置。
	if forgetDays <= blurDays {
		return fmt.Errorf(prompt.T(ictx.Lang, "prompt.memory.err.forget_le_blur",
			"「移出记忆库天数」（%d）需大于「细节淡忘天数」（%d）"), forgetDays, blurDays)
	}

	// 先停掉上一份配置起的后台活动：Init 会被 SetConfig 反复调用
	p.stopWorkers()

	p.storesMu.Lock()
	p.stores = map[string]*Store{}
	p.storesMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.lang = ictx.Lang
	p.library = library
	p.libBase = dir
	p.store = NewStore(ictx.Lang, dir)
	p.complete = ictx.Complete
	p.dayFacts = ictx.DayFacts
	p.notice = ictx.Notice
	p.maxIndexEntries = plugin.CfgInt(cfg, "max_index_entries", defaultMaxIndexEntries)
	p.maxIndexBytes = plugin.CfgInt(cfg, "max_index_bytes", defaultMaxIndexBytes)
	p.maxEntryBytes = plugin.CfgInt(cfg, "max_entry_bytes", defaultMaxEntryBytes)
	p.autoExtract = plugin.CfgBool(cfg, "auto_extract", defaultAutoExtract)
	p.maxExtract = plugin.CfgInt(cfg, "max_extract", defaultMaxExtract)
	p.turnExtract = plugin.CfgBool(cfg, "turn_extract", defaultTurnExtract)
	p.turnEvery = plugin.CfgInt(cfg, "turn_extract_every", defaultTurnEvery)
	p.decay = plugin.CfgBool(cfg, "decay", defaultDecay)
	p.blurDays = blurDays
	p.forgetDays = forgetDays
	p.timeline = plugin.CfgBool(cfg, "timeline", defaultTimeline)
	p.timelineDays = plugin.CfgInt(cfg, "timeline_days", defaultTimelineDays)
	p.recallMax = plugin.CfgInt(cfg, "recall_max", defaultRecallMax)
	p.recallEntryRunes = plugin.CfgInt(cfg, "recall_entry_runes", defaultRecallEntryRunes)
	p.recallMaxBytes = plugin.CfgInt(cfg, "recall_max_bytes", defaultRecallMaxBytes)
	p.recallMinScore = plugin.CfgInt(cfg, "recall_min_score", defaultRecallMinScore)
	p.ctx, p.cancel = context.WithCancel(context.Background())

	p.turnMu.Lock()
	p.stopped = false
	p.windowDir = ictx.StateDir
	// 只在首次装回：SetConfig 会重新 Init，那时内存里的缓冲比盘上的新，
	// 再装一次等于把刚攒的几轮退回旧值
	if !p.loaded {
		p.loaded = true
		if windows, marks := loadWindowState(p.windowDir); windows != nil {
			p.windows, p.lastSweep, p.lastTimeline = windows, marks.lastSweep, marks.lastTimeline
		}
	}
	p.turnMu.Unlock()
	return nil
}

// Stop 取消在途的提炼与清扫并有界等待。禁用插件、以新配置重新 Init、进程退出时被调用。
func (p *Plugin) Stop() { p.stopWorkers() }

// stopWorkers 取消后台 ctx 并等待已起的 goroutine 收尾，超过 stopWait 就不再等
// ——ctx 已经取消，在途的模型调用会自己断开。
func (p *Plugin) stopWorkers() {
	p.turnMu.Lock()
	p.stopped = true
	p.turnMu.Unlock()

	p.mu.Lock()
	cancel := p.cancel
	p.cancel, p.ctx = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopWait):
	}
}

func (p *Plugin) Tools() []plugin.Tool {
	return []plugin.Tool{
		&saveTool{p: p},
		&recallTool{p: p},
		&listTool{p: p},
		&deleteTool{p: p},
	}
}

// settings 是一次调用期间使用的配置快照。
type settings struct {
	lang             string
	store            *Store
	libBase          string
	maxIndexEntries  int
	maxIndexBytes    int
	maxEntryBytes    int
	autoExtract      bool
	maxExtract       int
	maxExtractBytes  int
	turnExtract      bool
	turnEvery        int
	decay            bool
	blurDays         int
	forgetDays       int
	timeline         bool
	timelineDays     int
	recallMax        int
	recallEntryRunes int
	recallMaxBytes   int
	recallMinScore   int
	ctx              context.Context
}

// snapshot 取一份配置快照：SetConfig 会在运行时重新 Init，而工具可能正在执行。
func (p *Plugin) snapshot() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return settings{
		lang:             p.lang,
		store:            p.store,
		libBase:          p.libBase,
		maxIndexEntries:  p.maxIndexEntries,
		maxIndexBytes:    p.maxIndexBytes,
		maxEntryBytes:    p.maxEntryBytes,
		autoExtract:      p.autoExtract,
		maxExtract:       p.maxExtract,
		maxExtractBytes:  maxExtractBytes,
		turnExtract:      p.turnExtract,
		turnEvery:        p.turnEvery,
		decay:            p.decay,
		blurDays:         p.blurDays,
		forgetDays:       p.forgetDays,
		timeline:         p.timeline,
		timelineDays:     p.timelineDays,
		recallMax:        p.recallMax,
		recallEntryRunes: p.recallEntryRunes,
		recallMaxBytes:   p.recallMaxBytes,
		recallMinScore:   p.recallMinScore,
		ctx:              p.ctx,
	}
}

// langOf 取进程语言。未初始化时是空串，prompt.T 据此回中文。
func (p *Plugin) langOf() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lang
}

// noticeFunc 返回当前的会话注记入口（可能为 nil，此时降级为只记日志）。
func (p *Plugin) noticeFunc() plugin.NoticeFunc {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.notice
}

// completeFunc 返回当前的辅助调用入口（可能为 nil）。
func (p *Plugin) completeFunc() plugin.CompleteFunc {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.complete
}

// dayFactsFunc 返回「问某一天有什么可说的」的入口（可能为 nil）。
func (p *Plugin) dayFactsFunc() plugin.DayFactsFunc {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.dayFacts
}

// sortByUpdatedDesc 按更新时间倒序，仅用于超预算时挑选保留哪些条目。
func sortByUpdatedDesc(entries []Entry) {
	slices.SortStableFunc(entries, func(a, b Entry) int {
		return b.Updated.Compare(a.Updated)
	})
}

// promptHeaderZH 领起本轮注入的记忆索引。它与 promptGuideZH 各占一个带标题的块：
// 一个是「已经记住了什么」，一个是「什么该记下来」，混在一个标题下模型分不清
// 哪句是清单、哪句是判据。
const promptHeaderZH = `[已保存的记忆]
以下是已保存的记忆条目，跨会话长期有效。需要某条的细节时用 recall_memory 按标题读取全文。`

// promptGuideZH 是保存判据。
//
// 标题不能省。其余插件注入的都是 [场景感知] [心情] 这样的带标题块，这一段早先是
// 全篇唯一一段无标题的散文，又排在 [角色设定 · 最高优先级] 之前，那句「以下设定优先
// 于其它一般性指令」正好把它归进了「其它一般性指令」。实测的表现是：同一批对话里
// adjust_mood 与 record_touch 都被调用过，save_memory 一次也没有。
//
// 各类的定义也刻意写成兼顾生活与工作。早先「偏好=工作方式要求、约定=项目规则、
// 事实=环境配置结构、踩坑=失败原因」是纯工程语境的，「我早上十点上班」这种话
// 落不进任何一类，模型于是不存——判据自己把最常见的一类记忆排除在外了。
// 「经历」是后加的第五类：前四类全是关于对方的，角色自己过的日子（心跳里的生活、
// 独处时做的事）此前沉淀不成自己的回忆。
// 「边界」是第六类：明确表达过的不愿意与坚持，此前无处可归，而它恰恰要求长期精确
// ——被归进「经历」的话会随生活片段一起淡忘，立场就在无声中变了。它与「经历」的
// 分界必须写出来，否则一次拒绝完全符合「自己遇到过的事」的定义，会被前一类吸走。
const promptGuideZH = `[长期记忆]
对话中出现下列内容时，用 save_memory 保存，使其在之后的会话中依然可用：
- 偏好：对方明确表达的、长期有效的喜好与要求——喜欢什么、不喜欢什么、希望怎么
  相处、希望怎么表达
- 约定：两人之间或协作上定下来的规则与安排
- 事实：关于对方及其处境的、不易重新得知的信息——身份、作息、工作、住处、家人、
  健康状况，以及环境、配置、结构这类
- 踩坑：已经验证过的失败原因与正确做法
- 经历：自己做过、遇到过的事——和谁去了哪、生活里发生了什么、独处时做了什么；主语
  是自己而不是对方，这是它与「事实」的分界
- 边界：任何一方明确表达过的不愿意与坚持——拒绝过什么、坚持过什么立场。谁表达的、
  对什么、因为什么都要写进正文；摘要只写对什么事的立场，不复述当时的对话。说出口
  的才算，委婉带开话头的不算；自己表达的边界归这一类，不归「经历」
对方说「记住……」「别忘了……」时一律照办，不要自己判断值不值得——他既然专门说了，
就是要你记住。日常生活里的近况、心情、当下的处境与安排同样属于上面的分类，照样保存。
不保存一次性的任务细节、临时中间结果与可以随时重新读取的内容。
定在某天某时的单次安排（明天下午三点见面、周六去看展）不存为记忆，归日程，过后由时间线记经历。
已有记忆的结论被推翻时，用同一个标题修订它（save_memory 的 mode 设为 replace），
并在正文里交代被推翻的旧结论；不要换个说法另存一条，那会让两条互相打架的记忆同时留着。
对话历史被压缩后，摘要中若含上述内容，需要检查是否已经保存。
[想起来的事] 是随对话自然浮现的记忆正文，直接用，不必再 recall_memory；没列出的仍可查。`

// decayPromptZH 只在开启淡忘时追加。
const decayPromptZH = `
保存时还要判断这条记忆会不会随时间失去意义：近况、心情、当下的处境与安排属于会失去
意义的，把 decay 设为 true，它们久未提及会逐步淡忘直至移出记忆库；稳定的偏好、定下的
约定、不易重新得知的事实、验证过的踩坑、表达过的边界不要设。
读到只剩一句要点的记忆时，说明它的细节已经淡忘了，就照这个程度说，不要凭空补细节。`

// SystemPrompt 只返回静态的保存判据。
//
// 记忆索引改由 TurnPrompt 注入：索引的内容取决于本轮的可见域，而 SystemPrompt 拿不到
// 可见域，一律全列会把不可读域的记忆标题漏出去。顺带的好处是本方法不再有任何磁盘
// 访问——列表接口会对禁用的插件也调用它。
func (p *Plugin) SystemPrompt() string {
	s := p.snapshot()
	if s.decay {
		return guideFor(s.lang) + "\n" + decayFor(s.lang)
	}
	return guideFor(s.lang)
}

// TurnPrompt 注入本轮自动想起的记忆正文、可读的记忆索引与时间线。
// 都为空时不注入：判据已在 SystemPrompt 里，那是引导保存第一条记忆的东西。
func (p *Plugin) TurnPrompt(ctx context.Context, ev plugin.TurnEvent) (string, error) {
	s := p.snapshot()
	if s.store == nil {
		return "", nil
	}
	entries, err := p.visibleEntries(ctx)
	if err != nil {
		return "", err
	}
	var parts []string
	// 召回块排在索引之前：索引是目录，召回是内容，先看到内容再看到目录更顺。
	// 被召回的条目在索引里照样列着，两处各司其职，不互相解释。
	if block := p.recallBlock(ctx, s, ev.UserInput, entries); block != "" {
		parts = append(parts, block)
	}
	if len(entries) > 0 {
		parts = append(parts, headerFor(s.lang)+"\n"+renderIndex(s.lang, entries, s.maxIndexEntries, s.maxIndexBytes))
	}
	if block := p.timelineBlock(ctx, s); block != "" {
		parts = append(parts, block)
	}
	return strings.Join(parts, "\n\n"), nil
}

// recallBlock 用本轮输入从可读的记忆里挑出最相关的几条，渲染成 [想起来的事]。
// entries 已经按可见域过滤过——不可读域的记忆连标题都不该露，正文更不能。
//
// 机器注入的一次性输入（心跳、定时任务的提示词）不召回：那段文字每次都差不多，
// 按它召回只会每拍都想起同样几条，白占上下文。
func (p *Plugin) recallBlock(ctx context.Context, s settings, input string, entries []Entry) string {
	if s.recallMax <= 0 || len(entries) == 0 || plugin.IsEphemeralInput(ctx) {
		return ""
	}
	hits := pickRecalls(input, entries, s.recallMax, s.recallMinScore)
	block, n := renderRecalls(s.lang, hits, s.recallEntryRunes, s.recallMaxBytes)
	if n == 0 {
		return ""
	}
	// 想起来就算用到：不刷新的话，一条天天自然浮现的记忆照样会走到淡忘。
	// 与 recall_memory 同一口径，只在开了淡忘时写——没人读这个时间就不值得为它写盘。
	if s.decay {
		now := time.Now()
		for _, h := range hits[:n] {
			if store := p.storeFor(h.entry.Domain); store != nil {
				_, _ = store.Touch(h.entry.Name, now)
			}
		}
	}
	return block
}

// renderIndex 按预算渲染索引，分三级降级：
// 全列（日期 + 分类/标题 + 摘要）→ 省略摘要只留标题 → 按最近更新截断并注明剩余条数。
// 降级优先砍摘要而不是砍条目：标题在，模型才知道这条记忆存在，还能按名读取。
func renderIndex(lang string, entries []Entry, maxEntries, maxBytes int) string {
	kept, dropped := limitEntries(entries, maxEntries)

	if out, ok := renderLines(lang, kept, dropped, maxBytes, true); ok {
		return out
	}
	if out, ok := renderLines(lang, kept, dropped, maxBytes, false); ok {
		return out
	}
	// 仍然超预算：按最近更新丢弃更多条目，直到装得下
	for n := len(kept) - 1; n > 0; n-- {
		trimmed, more := limitEntries(entries, n)
		if out, ok := renderLines(lang, trimmed, more, maxBytes, false); ok {
			return out
		}
	}
	return prompt.Tf(lang, "prompt.memory.index.over_budget",
		"（共 %d 条记忆，索引预算不足以列出，可用 list_memories 查看）", len(entries))
}

// limitEntries 在超出条数上限时按最近更新保留，返回保留的条目（仍是原顺序）与被略去的条数。
func limitEntries(entries []Entry, max int) ([]Entry, int) {
	if max <= 0 || len(entries) <= max {
		return entries, 0
	}
	recent := make([]Entry, len(entries))
	copy(recent, entries)
	// 只用更新时间挑选保留哪些，不用它决定展示顺序：
	// 按时间排序会让每次保存都重排整个提示词前缀。
	sortByUpdatedDesc(recent)
	keep := map[string]bool{}
	for _, e := range recent[:max] {
		keep[e.Slug] = true
	}
	out := make([]Entry, 0, max)
	for _, e := range entries {
		if keep[e.Slug] {
			out = append(out, e)
		}
	}
	return out, len(entries) - len(out)
}

func renderLines(lang string, entries []Entry, dropped, maxBytes int, withDesc bool) (string, bool) {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString("- ")
		// 用最后更新时间而不是创建时间：一条被反复修订的记忆，模型该看到的是
		// 这个结论最后一次被确认是什么时候，而不是这件事第一次被提起是什么时候。
		b.WriteString(e.Updated.Format("2006-01-02"))
		b.WriteString(" ")
		b.WriteString(memTypes.For(lang, e.Type))
		b.WriteString("/")
		b.WriteString(e.Name)
		if withDesc && e.Description != "" {
			b.WriteString(" — ")
			b.WriteString(e.Description)
		}
		b.WriteString("\n")
	}
	if dropped > 0 {
		b.WriteString(prompt.Tf(lang, "prompt.memory.index.more",
			"（另有 %d 条未列出，可用 list_memories 查看）", dropped) + "\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	if maxBytes > 0 && len(out) > maxBytes {
		return "", false
	}
	return out, true
}
