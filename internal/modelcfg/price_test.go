package modelcfg

import (
	"math"
	"os"
	"testing"

	"wen/internal/config"
	"wen/internal/usage"
)

// 价格是给人填的，校验要挡住这些输入：没有币种的金额（账本里只能算未定价）、
// 拼错的币种（同一个币种在合计里裂成两行）、负数金额（几乎总是小数点点错了）、
// 以及 NaN / ±Inf（写不进 JSON，会让用量报告编码失败）。有限但极大的值照旧放行。
func TestPriceValidation(t *testing.T) {
	cases := []struct {
		name  string
		price *usage.Price
	}{
		{"币种为空", &usage.Price{Input: 1, Output: 2}},
		{"币种只有空白", &usage.Price{Currency: "   ", Input: 1, Output: 2}},
		{"币种过长", &usage.Price{Currency: "ABCDEFGHI", Input: 1, Output: 2}},
		{"币种含空格", &usage.Price{Currency: "U SD", Input: 1, Output: 2}},
		{"输入价为负", &usage.Price{Currency: "USD", Input: -0.1, Output: 2}},
		{"输出价为负", &usage.Price{Currency: "USD", Input: 1, Output: -2}},
		{"缓存读价为负", &usage.Price{Currency: "USD", Input: 1, Output: 2, CacheRead: floatp(-1)}},
		{"缓存写价为负", &usage.Price{Currency: "USD", Input: 1, Output: 2, CacheWrite: floatp(-1)}},
		{"输入价为 NaN", &usage.Price{Currency: "USD", Input: math.NaN(), Output: 2}},
		{"输出价为 +Inf", &usage.Price{Currency: "USD", Input: 1, Output: math.Inf(1)}},
		{"缓存读价为 NaN", &usage.Price{Currency: "USD", Input: 1, Output: 2, CacheRead: floatp(math.NaN())}},
		{"缓存写价为 -Inf", &usage.Price{Currency: "USD", Input: 1, Output: 2, CacheWrite: floatp(math.Inf(-1))}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, path := newStore(t, baseConfig())
			next := s.Snapshot()
			next.Providers[0].Models[0].Price = c.price
			if _, err := s.Save("", next); err == nil {
				t.Fatalf("%s 应当被拒绝", c.name)
			}
			// 失败的保存不落盘：写坏的半份价格不该留在 models.json 里。
			if _, err := os.Stat(path); err == nil {
				t.Fatal("失败的保存不该写盘")
			}
		})
	}

	// 合法价格要放行：前后带空白的币种按 trim 后的值算（手填时很常见）；有限但极大的
	// 单价也是合法输入——金额一侧自己饱和（见 usage.Price.Cost），校验这一层不拒。
	for _, price := range []*usage.Price{
		{Currency: " USD ", Input: 0, Output: 0},
		{Currency: "USD", Input: 1e308, Output: 1e308, CacheRead: floatp(1e308), CacheWrite: floatp(1e308)},
	} {
		s, _ := newStore(t, baseConfig())
		next := s.Snapshot()
		next.Providers[0].Models[0].Price = price
		if _, err := s.Save("", next); err != nil {
			t.Fatalf("合法价格 %+v 被拒绝: %v", price, err)
		}
	}
}

// 币种落盘前统一成大写去空白：它是金额的分桶键，手填的 " usd " 与预算里的 "USD"
// 必须落进同一桶，界面徽标也不该跟着用户敲的大小写变。重新加载后仍是归一化后的值，
// 说明归一化发生在写盘之前而不只是内存里。
func TestPriceCurrencyNormalizedOnSave(t *testing.T) {
	cfg := baseConfig()
	s, path := newStore(t, cfg)
	next := s.Snapshot()
	next.Providers[0].Models[0].Price = &usage.Price{Currency: " usd ", Input: 1, Output: 2}
	if _, err := s.Save("", next); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2, err := NewStore("", path, cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	got, ok := s2.Price(next.Providers[0].Name, next.Providers[0].Models[0].ID)
	if !ok {
		t.Fatal("保存过的价格应当查得到")
	}
	if got.Currency != "USD" {
		t.Fatalf("重新加载后的币种 = %q, want USD", got.Currency)
	}
}

// 价格只存在 models.json 的模型条目上，所以「查得到」要同时覆盖覆盖层自己的提供商
// 与被它接管的 config.yaml 提供商；同名模型在两家下必须各查各的。
func TestPriceLookup(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers["zhipu"] = config.ProviderConfig{Type: "openai_compat", BaseURL: "https://open.bigmodel.cn", APIKey: "sk-z"}
	s, path := newStore(t, cfg)

	next := s.Snapshot()
	for i := range next.Providers {
		if next.Providers[i].Name == "deepseek" { // 来自 config.yaml，被这次保存接管
			next.Providers[i].Models[0].Price = &usage.Price{Currency: "CNY", Input: 2, Output: 8, CacheRead: floatp(0.2)}
		}
	}
	next.Providers = append(next.Providers,
		Provider{Name: "openai", Type: "openai_compat", BaseURL: "https://api.openai.com", APIKey: "sk-o",
			Models: []Model{{ID: "gpt-x", Price: &usage.Price{Currency: "USD", Input: 1.25, Output: 10}}}},
		// 同名模型在两家下各有一个价：查价必须带上提供商，不能只按模型名找。
		Provider{Name: "alpha", Type: "openai_compat", BaseURL: "https://a.example.com", APIKey: "k",
			Models: []Model{{ID: "shared", Price: &usage.Price{Currency: "CNY", Input: 1, Output: 2}}}},
		Provider{Name: "beta", Type: "openai_compat", BaseURL: "https://b.example.com", APIKey: "k",
			Models: []Model{{ID: "shared", Price: &usage.Price{Currency: "USD", Input: 3, Output: 4}}}},
	)
	if _, err := s.Save("", next); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 重新打开一次：价格得真的落在 models.json 里，而不是只在内存里。
	s2, err := NewStore("", path, cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, tc := range []struct {
		provider, model string
		want            usage.Price
	}{
		{"deepseek", "deepseek-chat", usage.Price{Currency: "CNY", Input: 2, Output: 8, CacheRead: floatp(0.2)}},
		{"openai", "gpt-x", usage.Price{Currency: "USD", Input: 1.25, Output: 10}},
		{"alpha", "shared", usage.Price{Currency: "CNY", Input: 1, Output: 2}},
		{"beta", "shared", usage.Price{Currency: "USD", Input: 3, Output: 4}},
		// 提供商名沿用 findProvider 的大小写不敏感口径，模型 ID 仍要精确匹配。
		{"DeepSeek", "deepseek-chat", usage.Price{Currency: "CNY", Input: 2, Output: 8, CacheRead: floatp(0.2)}},
	} {
		got, ok := s2.Price(tc.provider, tc.model)
		if !ok {
			t.Fatalf("Price(%q, %q) 应当查得到", tc.provider, tc.model)
		}
		if got.Currency != tc.want.Currency || got.Input != tc.want.Input || got.Output != tc.want.Output ||
			(got.CacheRead == nil) != (tc.want.CacheRead == nil) ||
			(got.CacheRead != nil && *got.CacheRead != *tc.want.CacheRead) {
			t.Fatalf("Price(%q, %q) = %+v, want %+v", tc.provider, tc.model, got, tc.want)
		}
	}

	// 没填价格、模型 ID 拼错、提供商不存在，都返回 false——不能返回零值 Price，
	// 那会被当成「免费」。
	for _, tc := range []struct{ provider, model string }{
		{"deepseek", "custom"},     // 该提供商下没这个模型
		{"zhipu", "deepseek-chat"}, // config.yaml 里的提供商，没有接管、也没有价格
		{"nope", "shared"},
		{"alpha", "SHARED"}, // 模型 ID 精确匹配，大小写不同就是另一个模型
	} {
		if p, ok := s2.Price(tc.provider, tc.model); ok {
			t.Fatalf("Price(%q, %q) 不该查得到: %+v", tc.provider, tc.model, p)
		}
	}
}

// PriceSnapshot 是给整月扫描用的价格快照：取完就与 Store 脱钩。报告页扫一份账本要
// 查成千上万次价，若期间保存了一次模型（改价），逐条现查会让同一份报表前半截旧价、
// 后半截新价。这里断言快照确实「冻结」住了：保存之后旧快照仍是旧价，新快照才是新价；
// 同时口径与 Price 一致——config.yaml 侧的提供商查得到、没定价的模型查不到。
func TestPriceSnapshot(t *testing.T) {
	cfg := baseConfig()
	cfg.Providers["zhipu"] = config.ProviderConfig{Type: "openai_compat", BaseURL: "https://open.bigmodel.cn", APIKey: "sk-z"}
	s, _ := newStore(t, cfg)

	// setPrice 在当前视图上给某一对（提供商, 模型）换价并保存，价格照旧整档提交。
	setPrice := func(provider, model string, price *usage.Price) {
		t.Helper()
		doc := s.Snapshot()
		found := false
		for i := range doc.Providers {
			if doc.Providers[i].Name != provider {
				continue
			}
			for j := range doc.Providers[i].Models {
				if doc.Providers[i].Models[j].ID == model {
					doc.Providers[i].Models[j].Price = price
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("视图里没有 %s / %s", provider, model)
		}
		if _, err := s.Save("", doc); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	// deepseek 来自 config.yaml（这家被本次保存接管，价格记在 models.json 里）；
	// alpha 只在 models.json 里，其中 one 有价、free 没定价。
	doc := s.Snapshot()
	for i := range doc.Providers {
		if doc.Providers[i].Name == "deepseek" {
			doc.Providers[i].Models[0].Price = &usage.Price{Currency: "CNY", Input: 2, Output: 8}
		}
	}
	doc.Providers = append(doc.Providers,
		Provider{Name: "alpha", Type: "openai_compat", BaseURL: "https://a.example.com", APIKey: "k",
			Models: []Model{
				{ID: "one", Price: &usage.Price{Currency: "USD", Input: 1.25, Output: 10, CacheRead: floatp(0.2)}},
				{ID: "free"},
			}},
	)
	if _, err := s.Save("", doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	old := s.PriceSnapshot()
	if p, ok := old("deepseek", "deepseek-chat"); !ok || p.Currency != "CNY" || p.Input != 2 {
		t.Fatalf("快照应能查到 config.yaml 侧提供商的价格: %+v (存在=%v)", p, ok)
	}
	// 提供商名与 Price 一样大小写不敏感，模型 ID 精确匹配。
	if p, ok := old("ALPHA", "one"); !ok || p.Input != 1.25 {
		t.Fatalf("快照应按提供商查到 alpha/one: %+v (存在=%v)", p, ok)
	}
	if _, ok := old("alpha", "free"); ok {
		t.Fatal("没填价格的模型不该出现在快照里——那会被当成 0 元")
	}
	if _, ok := old("alpha", "ONE"); ok {
		t.Fatal("模型 ID 精确匹配，大小写不同就是另一个模型")
	}
	if _, ok := old("zhipu", "deepseek-chat"); ok {
		t.Fatal("config.yaml 里没有价格的提供商不该查得到")
	}

	// 扫描期间改价：已取出的快照不受影响，重新取才看到新价。
	setPrice("deepseek", "deepseek-chat", &usage.Price{Currency: "CNY", Input: 5, Output: 20})
	setPrice("alpha", "one", &usage.Price{Currency: "USD", Input: 9, Output: 30, CacheRead: floatp(0.3)})

	if p, ok := old("deepseek", "deepseek-chat"); !ok || p.Input != 2 {
		t.Fatalf("保存改价后旧快照应仍是旧价: %+v (存在=%v)", p, ok)
	}
	if p, _ := old("alpha", "one"); p.Input != 1.25 || p.CacheRead == nil || *p.CacheRead != 0.2 {
		t.Fatalf("保存改价后旧快照的缓存价应仍是旧值: %+v", p)
	}
	fresh := s.PriceSnapshot()
	if p, ok := fresh("deepseek", "deepseek-chat"); !ok || p.Input != 5 {
		t.Fatalf("新快照应看到新价: %+v (存在=%v)", p, ok)
	}
	if p, ok := fresh("alpha", "one"); !ok || p.Input != 9 {
		t.Fatalf("新快照应看到 alpha 的新价: %+v (存在=%v)", p, ok)
	}
}

// 两家名字只差大小写、第一家没定价时，快照必须像 Price 一样选中第一家（未定价），
// 不能跳过它去取第二家的价——同一条账本记录不能实时按未定价、报表按已定价。
// 正常保存会拒绝这种重名（validateDoc 按 EqualFold 判），这里直接构造视图，
// 模拟旧版本留下的文件或手工编辑的配置。
func TestPriceSnapshotPicksSameProviderAsPrice(t *testing.T) {
	s := &Store{own: File{Providers: []Provider{
		{Name: "Ζευς", Type: "openai_compat", BaseURL: "https://a.example.com",
			Models: []Model{{ID: "m"}}},
		{Name: "ΖΕΥΣ", Type: "openai_compat", BaseURL: "https://b.example.com",
			Models: []Model{{ID: "m", Price: &usage.Price{Currency: "USD", Input: 9, Output: 30}}}},
	}}}

	if p, ok := s.Price("Ζευς", "m"); ok {
		t.Fatalf("实时查价应选中第一家的未定价模型，得到 %+v", p)
	}
	for _, name := range []string{"Ζευς", "ΖΕΥΣ", "ζευς"} {
		if p, ok := s.PriceSnapshot()(name, "m"); ok {
			t.Fatalf("快照按 %q 查价应与实时查价一致（第一家没定价），得到 %+v", name, p)
		}
	}

	// 第一家有价时，快照取第一家的价，而不是列表里更靠后的同名提供商。
	s.own.Providers[0].Models[0].Price = &usage.Price{Currency: "CNY", Input: 1, Output: 2}
	if p, ok := s.PriceSnapshot()("ΖΕΥΣ", "m"); !ok || p.Currency != "CNY" || p.Input != 1 {
		t.Fatalf("快照应取第一家（按 EqualFold 命中的那个）的价，得到 %+v (存在=%v)", p, ok)
	}
}

// samePrice 比较两份单价的全部字段（缓存价是指针，逐值比）。
func samePrice(got, want usage.Price) bool {
	if got.Currency != want.Currency || got.Input != want.Input || got.Output != want.Output {
		return false
	}
	return sameFloat(got.CacheRead, want.CacheRead) && sameFloat(got.CacheWrite, want.CacheWrite)
}

func sameFloat(a, b *float64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// 快照的提供商名匹配必须与 Price 完全一致。Price 用 strings.EqualFold（见 findProvider），
// 而「折成小写再比」与它并不等价：希腊字母的 σ 与词尾 ς 是同一个字母的大小写折叠，
// ToLower 却把它们折成两串。此前快照就是这么造的键，于是以 Ζευς 定价的历史用量，
// 一旦按 ΖΕΥΣ 查价就成了「未定价」——token 还在合计里，钱却不见了。
func TestPriceSnapshotProviderNameMatchesPrice(t *testing.T) {
	const (
		greek = "Ζευς" // 按这个写法定价；查询用大写 ΖΕΥΣ 与全小写 ζευς
		ascii = "Hermes"
	)
	s, _ := newStore(t, baseConfig())

	doc := s.Snapshot()
	doc.Providers = append(doc.Providers,
		Provider{Name: greek, Type: "openai_compat", BaseURL: "https://zeus.example.com", APIKey: "k",
			Models: []Model{
				{ID: "olympus", Price: &usage.Price{Currency: "USD", Input: 1.5, Output: 6, CacheRead: floatp(0.15)}},
				{ID: "nectar"}, // 没定价
			}},
		Provider{Name: ascii, Type: "openai_compat", BaseURL: "https://hermes.example.com", APIKey: "k",
			Models: []Model{{ID: "wing", Price: &usage.Price{Currency: "USD", Input: 2, Output: 8}}}},
	)
	if _, err := s.Save("", doc); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 快照取在后面的改价之前：它该按取快照那一刻的价表回答。
	snap := s.PriceSnapshot()

	for _, tc := range []struct{ provider, model string }{
		{"ΖΕΥΣ", "olympus"}, // σ 换成词尾 ς：ToLower 折不出来，EqualFold 认得
		{"ζευς", "olympus"},
		{"HERMES", "wing"}, // 普通 ASCII 的大小写同样如此
		{"hermes", "wing"},
	} {
		want, ok := s.Price(tc.provider, tc.model)
		if !ok {
			t.Fatalf("Price(%q, %q) 应当查得到（快照要对齐的就是它）", tc.provider, tc.model)
		}
		got, ok := snap(tc.provider, tc.model)
		if !ok {
			t.Fatalf("快照应查得到 %q / %q——与 Price 同口径", tc.provider, tc.model)
		}
		if !samePrice(got, want) {
			t.Fatalf("快照给 %q / %q 的价 = %+v, want %+v（与 Price 相同）", tc.provider, tc.model, got, want)
		}
	}

	// 没定价的模型、拼错的模型 ID、不存在的提供商仍是 false：模型 ID 精确匹配。
	for _, tc := range []struct{ provider, model string }{
		{"ΖΕΥΣ", "nectar"},
		{"ΖΕΥΣ", "OLYMPUS"},
		{"hermes", "WING"},
		{"nope", "olympus"},
	} {
		if p, ok := snap(tc.provider, tc.model); ok {
			t.Fatalf("快照不该查到 %q / %q: %+v", tc.provider, tc.model, p)
		}
	}

	// 改价之后，先前取出的快照仍按旧价回答（冻结），新快照才看到新价。
	doc = s.Snapshot()
	for i := range doc.Providers {
		if doc.Providers[i].Name != greek {
			continue
		}
		for j := range doc.Providers[i].Models {
			if doc.Providers[i].Models[j].ID == "olympus" {
				doc.Providers[i].Models[j].Price = &usage.Price{Currency: "USD", Input: 9, Output: 30}
			}
		}
	}
	if _, err := s.Save("", doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if p, ok := snap("ΖΕΥΣ", "olympus"); !ok || p.Input != 1.5 || p.CacheRead == nil || *p.CacheRead != 0.15 {
		t.Fatalf("改价后旧快照应仍是旧价: %+v (存在=%v)", p, ok)
	}
	if p, ok := s.PriceSnapshot()("ΖΕΥΣ", "olympus"); !ok || p.Input != 9 {
		t.Fatalf("新快照应看到新价: %+v (存在=%v)", p, ok)
	}
}
