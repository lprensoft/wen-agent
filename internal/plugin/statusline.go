package plugin

// StatusReporter 由需要在状态命令中露出一行运行状况的插件实现（可选）。
// 核心只负责按注册顺序把启用插件的非空行拼进状态输出，不解释内容——
// 它不知道「心跳」「定时任务」这回事，插件自己决定要不要说、说什么。
//
// 契约与 SystemPrompt / Actions 相同：必须廉价、无副作用、可随时调用——它在
// 每次状态查询时被调用，实现里不要做网络请求或大段计算。反向调用 Manager 不再
// 死锁（回调都在锁外进行），但别在 StatusLines 里回头问 InitContext.Status：
// 那条路径最终又会回到 StatusLines，是无限递归。也不要在 Init 内查状态——
// Init 仍在写锁内调用。
//
// 措辞由插件负责：一行一条、自带前缀（如「💓 心跳：…」）。状态输出是给人扫一眼的，
// 不要塞多行长文，也不要放随时间抖动到看不懂的精确值。
type StatusReporter interface {
	// lang 是要用哪门语言写这一行。它按调用方判定：Web UI 的 /api/status 用那次
	// 请求的语言，IM 通道的 /status 与 wen status 用进程语言（那两处没有请求可
	// 依附）。插件不要自己去存语言——同一个服务可能同时连着一个中文浏览器和一个
	// 英文的终端。
	StatusLines(lang string) []string
}
