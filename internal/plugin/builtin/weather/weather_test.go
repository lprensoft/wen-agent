package weather

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"wen/internal/plugin"
)

// setObs 直接往缓存里塞一份观测，免得每个测试都去过一遍网络。
func setObs(p *Plugin, loc string, rep Report) {
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	o := p.entryLocked(loc)
	o.cur, o.curOK = rep, true
}

func report(place, cond string, temp float64, age time.Duration) Report {
	return Report{Place: place, Condition: cond, TempC: temp, Fetched: time.Now().Add(-age)}
}

func TestRenderConditions(t *testing.T) {
	got := renderConditions("", Report{Condition: "小雨", TempC: 18.4, FeelsC: 16.9, Humidity: 82, WindKmh: 9.3})
	want := "小雨，18℃（体感 17℃），湿度 82%，风速 9 km/h。"
	if got != want {
		t.Errorf("渲染 = %q\n期望 = %q", got, want)
	}
}

// 体感与气温差不到一度时不写体感：一行里两个一样的数字只是噪声。
func TestRenderConditionsOmitsCloseApparent(t *testing.T) {
	got := renderConditions("", Report{Condition: "晴", TempC: 18, FeelsC: 18.2, Humidity: 40, WindKmh: 3})
	if strings.Contains(got, "体感") {
		t.Errorf("体感与气温接近时不应写出来: %q", got)
	}
}

// ---------- 两处地点的归一化 ----------

func TestNormalizeLocations(t *testing.T) {
	cases := []struct {
		name        string
		cfg         map[string]any
		wantPersona string
		wantUser    string
		wantSame    bool
	}{
		{
			name:        "同城时忽略我所在的城市",
			cfg:         map[string]any{"persona_location": "杭州", "same_city": true, "user_location": "上海"},
			wantPersona: "杭州", wantUser: "", wantSame: true,
		},
		{
			name:        "分处两地各算各的",
			cfg:         map[string]any{"persona_location": "杭州", "same_city": false, "user_location": "上海"},
			wantPersona: "杭州", wantUser: "上海", wantSame: false,
		},
		{
			// 否则会注入两行一模一样的天气，读起来像两个地方碰巧天气相同
			name:        "两处填了同一个地方就是同城",
			cfg:         map[string]any{"persona_location": "杭州", "same_city": false, "user_location": "杭州"},
			wantPersona: "杭州", wantUser: "", wantSame: true,
		},
		{
			name:        "只填我所在的城市也合法",
			cfg:         map[string]any{"persona_location": "", "same_city": false, "user_location": "上海"},
			wantPersona: "", wantUser: "上海", wantSame: false,
		},
		{
			name:        "两侧空白被裁掉",
			cfg:         map[string]any{"persona_location": "  杭州  ", "same_city": true},
			wantPersona: "杭州", wantUser: "", wantSame: true,
		},
	}
	for _, c := range cases {
		persona, user, same := normalizeLocations(c.cfg)
		if persona != c.wantPersona || user != c.wantUser || same != c.wantSame {
			t.Errorf("%s: = (%q, %q, %v)，期望 (%q, %q, %v)",
				c.name, persona, user, same, c.wantPersona, c.wantUser, c.wantSame)
		}
	}
}

func TestWantedLocationsDedups(t *testing.T) {
	if got := wantedLocations("杭州", ""); len(got) != 1 || got[0] != "杭州" {
		t.Errorf("= %v", got)
	}
	if got := wantedLocations("杭州", "杭州"); len(got) != 1 {
		t.Errorf("同一个城市不该取两次: %v", got)
	}
	if got := wantedLocations("", ""); len(got) != 0 {
		t.Errorf("都为空时不该有目标: %v", got)
	}
}

// ---------- 配置 ----------

func TestInitRejectsStaleShorterThanRefresh(t *testing.T) {
	p := New()
	defer p.Stop()
	err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "refresh_minutes": 60, "stale_minutes": 30,
	})
	if err == nil {
		t.Fatal("过期时限小于刷新间隔时应当拒绝：那样刚取到的天气也立刻过期，等于永不注入")
	}
}

func TestConfigDefaults(t *testing.T) {
	fields := New().ConfigFields()
	byKey := map[string]plugin.ConfigField{}
	for _, f := range fields {
		byKey[f.Key] = f
	}
	for _, k := range []string{"persona_location", "same_city", "user_location", "refresh_minutes", "stale_minutes"} {
		if _, ok := byKey[k]; !ok {
			t.Fatalf("缺少配置项 %q", k)
		}
	}
	if byKey["same_city"].Default != true {
		t.Error("默认应当同城：角色扮演多数发生在用户自己的世界里，也省掉一次请求")
	}
	if byKey["stale_minutes"].Default.(int) < byKey["refresh_minutes"].Default.(int) {
		t.Fatal("默认值自相矛盾：过期时限小于刷新间隔")
	}
}

// 一个城市都没填时不起后台循环，也不注入任何东西——包括那段讲天气怎么用的规则。
// 奇幻舞台就该是这个状态。
func TestNoLocationInjectsNothing(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"persona_location": "", "user_location": ""}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := p.SystemPrompt(); got != "" {
		t.Errorf("未设置城市时不应注入规则，实际 %q", got)
	}
	out, err := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if err != nil || out != "" {
		t.Errorf("未设置城市时不应注入天气，实际 %q / %v", out, err)
	}
}

func TestSystemPromptWithLocation(t *testing.T) {
	stubServer(t, geoOK, fcOK)
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"persona_location": "杭州"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	prompt := p.SystemPrompt()
	if !strings.Contains(prompt, "[天气感知]") {
		t.Fatal("设置了城市后应当注入使用规则")
	}
	// 规则的要害在于给角色自己的天气一个出口：进环境描写，而不是变成话题
	if !strings.Contains(prompt, "场景与动作的描写") {
		t.Error("规则里必须写明自己那边的天气进入环境描写，否则模型对它无事可做")
	}
}

// ---------- 注入 ----------

func TestTurnPromptSameCity(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "same_city": true, "stale_minutes": 60,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop()
	setObs(p, "杭州", report("杭州 · 浙江 · 中国", "小雨", 18, 0))

	out, err := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if err != nil {
		t.Fatalf("TurnPrompt: %v", err)
	}
	if !strings.Contains(out, "你与对方同在") {
		t.Errorf("同城时措辞应当合成一处，实际 %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("同城时只该有一行天气，实际 %q", out)
	}
}

func TestTurnPromptTwoCities(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "same_city": false, "user_location": "上海", "stale_minutes": 60,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop()
	setObs(p, "杭州", report("杭州 · 浙江 · 中国", "小雨", 18, 0))
	setObs(p, "上海", report("上海 · 上海市 · 中国", "阴", 27, 0))

	out, err := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if err != nil {
		t.Fatalf("TurnPrompt: %v", err)
	}
	// 「你所在的」必须是角色那一处：角色自己的天气才进得了它的环境
	if !strings.Contains(out, "你所在的杭州") {
		t.Errorf("角色那处应当写成「你所在的」，实际 %q", out)
	}
	if !strings.Contains(out, "对方所在的上海") {
		t.Errorf("用户那处应当写成「对方所在的」，实际 %q", out)
	}
}

// 一边取到、一边取不到时注入取到的那一边——半边可用不该让整块消失。
func TestTurnPromptOneSideStale(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "same_city": false, "user_location": "上海", "stale_minutes": 60,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop()
	setObs(p, "杭州", report("杭州 · 浙江 · 中国", "小雨", 18, 0))
	setObs(p, "上海", report("上海 · 上海市 · 中国", "阴", 27, 3*time.Hour))

	out, _ := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if !strings.Contains(out, "你所在的杭州") {
		t.Errorf("新鲜的那一边应当照常注入，实际 %q", out)
	}
	if strings.Contains(out, "上海") {
		t.Errorf("过期的那一边不该注入，实际 %q", out)
	}
}

func TestTurnPromptAllStaleInjectsNothing(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "stale_minutes": 60,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop()
	setObs(p, "杭州", report("杭州", "晴", 20, 3*time.Hour))

	out, _ := p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if out != "" {
		t.Errorf("全部过期时不该注入：让角色不知道天气，好过把旧天气当成此刻。实际 %q", out)
	}
}

// ---------- 后台刷新与缓存 ----------

func TestRefreshOncePopulatesCache(t *testing.T) {
	stubServer(t, geoOK, fcOK)
	p := New()
	defer p.Stop()
	p.refreshOne(context.Background(), http.DefaultClient, "杭州")

	rep, ok, _ := p.lastReport("杭州")
	if !ok {
		t.Fatal("刷新成功后应当有观测")
	}
	if rep.Condition != "小雨" || rep.Place != "杭州 · 浙江省 · 中国" {
		t.Errorf("观测 = %+v", rep)
	}
}

// 地名解析结果按城市缓存：它几乎不变，不该每次取天气都重解析一遍。
func TestGeocodeResultIsCached(t *testing.T) {
	hits, _ := countingStub(t)
	p := New()
	defer p.Stop()
	for i := 0; i < 3; i++ {
		p.refreshOne(context.Background(), http.DefaultClient, "杭州")
	}
	if *hits != 1 {
		t.Errorf("地名解析发生了 %d 次，应当只有 1 次", *hits)
	}
}

// 取不到新数据时保留上一份，由过期时限决定它还能不能用——而不是当场清空。
func TestRefreshFailureKeepsLastReport(t *testing.T) {
	stubServer(t, geoOK, "")
	p := New()
	defer p.Stop()
	setObs(p, "杭州", report("杭州", "晴", 20, 0))

	p.refreshOne(context.Background(), http.DefaultClient, "杭州")

	rep, ok, lastErr := p.lastReport("杭州")
	if !ok || rep.Condition != "晴" {
		t.Error("取数失败不该丢掉上一份观测")
	}
	if lastErr == "" {
		t.Error("失败原因应当记下来，状态行要靠它解释为什么没有天气")
	}
}

// 改配置时重新 Init 必须能停掉旧循环：两套定时器并行会双倍打接口。
func TestInitIsReentrant(t *testing.T) {
	stubServer(t, geoOK, fcOK)
	p := New()
	defer p.Stop()
	for i := 0; i < 3; i++ {
		if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{"persona_location": "杭州"}); err != nil {
			t.Fatalf("第 %d 次 Init: %v", i, err)
		}
	}
	p.Stop()
	p.Stop() // 重复 Stop 不应 panic
}

// 只丢掉不再需要的城市的缓存：留下的那一处仍然是对的，扔掉它可能白白空窗一段。
func TestInitKeepsCacheForStillWantedCities(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "same_city": false, "user_location": "上海",
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop()
	setObs(p, "杭州", report("杭州", "小雨", 18, 0))
	setObs(p, "上海", report("上海", "阴", 27, 0))

	// 改成同城：上海不再需要
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "same_city": true,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop()

	if _, ok, _ := p.lastReport("杭州"); !ok {
		t.Error("仍在用的城市不该丢缓存")
	}
	if _, ok, _ := p.lastReport("上海"); ok {
		t.Error("不再需要的城市应当从缓存里清掉")
	}
}

// ---------- 契约 ----------

func TestPluginContract(t *testing.T) {
	p := New()
	if p.Name() != "weather" {
		t.Errorf("插件名 = %q", p.Name())
	}
	if p.Category() != plugin.CategoryPersona {
		t.Errorf("分组 = %q：注入型的环境状态属于角色演绎，不是基础工具", p.Category())
	}
	if len(p.Requires()) != 1 || p.Requires()[0] != "roleplay" {
		t.Errorf("依赖 = %v：没有角色，一行天气只是上下文里的噪声", p.Requires())
	}
	if len(p.Tools()) != 0 {
		t.Error("本插件不提供工具：它注入的是环境状态，不是一个可调用的能力")
	}
	var _ plugin.Stoppable = p
	var _ plugin.TurnPrompter = p
	var _ plugin.Actionable = p
}

func TestMentionsTomorrow(t *testing.T) {
	for in, want := range map[string]bool{
		"明天会下雨吗": true, "明早要出门": true, "明晚一起吃饭": true, "明日复明日": true,
		"今天好热": false, "后天呢": false, "": false,
	} {
		if got := mentionsTomorrow("", in); got != want {
			t.Errorf("mentionsTomorrow(%q) = %v", in, got)
		}
	}
}

func TestTurnPromptShowsTomorrowOnlyInWindow(t *testing.T) {
	p := New()
	defer p.Stop()
	if err := p.Init(plugin.InitContext{StateDir: t.TempDir()}, map[string]any{
		"persona_location": "杭州", "same_city": true, "stale_minutes": 1440, "tomorrow_from_hour": 18,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	p.Stop()

	day := time.Now()
	at := func(h int) time.Time { return time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, time.Local) }
	const layout = "2006-01-02"
	r := report("杭州 · 浙江 · 中国", "多云", 24, 0)
	r.Fetched = at(9) // 观测时刻也绑在注入的时钟上，别被过期判定挡掉
	r.Today = DayInfo{Date: day.Format(layout), Condition: "多云", MinC: 20, MaxC: 28}
	r.Tomorrow = DayInfo{Date: day.AddDate(0, 0, 1).Format(layout), Condition: "中雨", MinC: 12, MaxC: 18}
	setObs(p, "杭州", r)

	// 上午：今天在、明天不在，也没给角色「看过」
	p.now = func() time.Time { return at(10) }
	out, _ := p.TurnPrompt(context.Background(), plugin.TurnEvent{UserInput: "今天好闷"})
	if !strings.Contains(out, "今天预计多云") || strings.Contains(out, "明天") {
		t.Errorf("白天不该有明天: %q", out)
	}
	if seen := p.obs["杭州"].cur.Tomorrow.Seen; !seen.IsZero() {
		t.Errorf("没注入就不该记为看过: %v", seen)
	}
	// 上午但对方问起明天：看一眼，并从此刻起算「看过」
	out, _ = p.TurnPrompt(context.Background(), plugin.TurnEvent{UserInput: "明天会下雨吗"})
	if !strings.Contains(out, "明天预计中雨") || strings.Contains(out, "早些时候") {
		t.Errorf("问起明天应注入且不该标早就知道了: %q", out)
	}
	if seen := p.obs["杭州"].cur.Tomorrow.Seen; !seen.Equal(at(10)) {
		t.Errorf("首次注入应记下时刻，得到 %v", seen)
	}
	// 傍晚：窗口内照常出现，距首次看到已超三小时 → 标早就知道了
	p.now = func() time.Time { return at(19) }
	out, _ = p.TurnPrompt(context.Background(), plugin.TurnEvent{})
	if !strings.Contains(out, "明天预计中雨，12~18℃（早些时候就知道了）") {
		t.Errorf("傍晚应注入并标早就知道了: %q", out)
	}
	// Seen 只记第一次，不被后来的注入覆盖
	if seen := p.obs["杭州"].cur.Tomorrow.Seen; !seen.Equal(at(10)) {
		t.Errorf("Seen 不该被覆盖，得到 %v", seen)
	}
}
