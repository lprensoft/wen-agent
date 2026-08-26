package plugin

// DayReporter 由能为**某一天**补一句客观事实的插件实现（可选）。
//
// 它与 StatusReporter 是同一族：插件贡献一行，核心只负责按注册顺序把非空的收集
// 起来，不解释内容——核心不知道「天气」，也不知道这些行最后会被拼成一篇日记。
// 谁要用这份汇总，经 InitContext.DayFacts 取（先例是 InitContext.Status：那里返回的
// PluginLines 同样是别家插件贡献的行）。
//
// date 是本地时区的 2006-01-02。答不上来（那天没有记录、功能没配置）就返回 nil，
// 不要编——补不上的那一天，宁可少一句。
//
// 契约与 StatusLines 相同：必须廉价、无副作用、可随时调用，不发网络请求、不调模型。
// 它会在按日收束这类后台路径上被调用，实现里只该查自己已经存着的东西；同样不要在
// DayFacts 里回头调 InitContext.DayFacts——那条路径会绕回这里，是无限递归。
type DayReporter interface {
	DayFacts(date string) []string
}

// DayFactsFunc 是 InitContext.DayFacts 的类型：问一天，拿回各插件贡献的行。
type DayFactsFunc func(date string) []string

// DayFacts 汇总启用插件对某一天的说法，按注册顺序排列。
func (m *Manager) DayFacts(date string) []string {
	var out []string
	for _, e := range enabledAs[DayReporter](m) {
		var lines []string
		safely(m.lang(), e.name, "DayFacts", func() { lines = e.impl.DayFacts(date) })
		for _, line := range lines {
			if line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}
