package usage

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSettingsFile 在临时目录里落一份 usage.json，返回路径。
func writeSettingsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if s.Version != 1 {
		t.Errorf("Version = %d, want 1", s.Version)
	}
	if s.MonthlyTokens != 0 || s.MonthlyCost != 0 {
		t.Errorf("限额默认应为 0（不设限），got %d / %v", s.MonthlyTokens, s.MonthlyCost)
	}
	if s.Currency != "CNY" {
		t.Errorf("Currency = %q, want CNY", s.Currency)
	}
	if s.WarnPercent != 80 {
		t.Errorf("WarnPercent = %d, want 80", s.WarnPercent)
	}
	if s.Over != OverSkipBackground {
		t.Errorf("Over = %q, want %q", s.Over, OverSkipBackground)
	}
	if err := ValidateSettings(s); err != nil {
		t.Errorf("默认值应当合法: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "usage.json")
	want := Settings{
		MonthlyTokens: 1_000_000,
		MonthlyCost:   12.5,
		Currency:      "USD",
		WarnPercent:   90,
		Over:          OverBlockAll,
	}
	if err := SaveSettings(path, want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	want.Version = 1 // 写出的永远是当前版本

	got, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}

	checkPerm(t, path, 0o600)
	checkPerm(t, filepath.Dir(path), 0o700)
}

func TestLoadSettingsMissingFile(t *testing.T) {
	got, err := LoadSettings(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if got != DefaultSettings() {
		t.Fatalf("got %+v, want defaults", got)
	}
}

func TestLoadSettingsFillsMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	body := `{"monthly_tokens": 5000, "currency": "", "over": ""}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("只写了几项不该报错: %v", err)
	}
	want := DefaultSettings()
	want.MonthlyTokens = 5000
	if got != want {
		t.Fatalf("got %+v, want %+v（没写的项应取默认值，空串按未设置处理）", got, want)
	}
}

func TestLoadSettingsRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"corrupt":          `{"monthly_tokens": 1000,`,
		"not object":       `[1, 2, 3]`,
		"negative tokens":  `{"monthly_tokens": -1}`,
		"negative cost":    `{"monthly_cost": -0.01}`,
		"warn too high":    `{"warn_percent": 101}`,
		"warn negative":    `{"warn_percent": -1}`,
		"unknown over":     `{"over": "block_somewhere"}`,
		"over wrong case":  `{"over": "BLOCK_ALL"}`,
		"tokens not int":   `{"monthly_tokens": "many"}`,
		"warn not integer": `{"warn_percent": 80.5}`,
		"empty file":       ``,
		"null body":        `null`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadSettings(path)
			if err == nil {
				t.Fatalf("应当报错，got %+v", got)
			}
			// 出错时绝不给半份配置。
			if got != DefaultSettings() {
				t.Fatalf("出错时应返回默认值，got %+v", got)
			}
		})
	}
}

func TestSaveSettingsNormalizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := SaveSettings(path, Settings{MonthlyTokens: 10}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.Version != 1 || got.Currency != "CNY" || got.Over != OverSkipBackground {
		t.Fatalf("空串与版本应被归一化，got %+v", got)
	}
	if got.MonthlyTokens != 10 {
		t.Fatalf("MonthlyTokens = %d, want 10", got.MonthlyTokens)
	}
}

func TestSaveSettingsRejectsInvalid(t *testing.T) {
	cases := map[string]Settings{
		"negative tokens": {MonthlyTokens: -1},
		"negative cost":   {MonthlyCost: -0.5},
		"warn too high":   {WarnPercent: 101},
		"warn negative":   {WarnPercent: -1},
		"unknown over":    {Over: "stop_everything"},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.json")
			if err := SaveSettings(path, s); err == nil {
				t.Fatalf("应当拒绝 %+v", s)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("校验失败不该留下文件: %v", err)
			}
		})
	}
}

func TestSaveSettingsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s := Settings{MonthlyTokens: 100, MonthlyCost: 1.25, Currency: "EUR", WarnPercent: 70, Over: OverBlockAll}
	if err := SaveSettings(path, s); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 原样再存一次，以及从读回的值再存一次，字节都该一致。
	if err := SaveSettings(path, s); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("重复保存改变了内容:\n%s\n---\n%s", first, second)
	}
	loaded, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(path, loaded); err != nil {
		t.Fatal(err)
	}
	third, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(third) {
		t.Fatalf("读回再保存改变了内容:\n%s\n---\n%s", first, third)
	}
}

func TestValidateSettings(t *testing.T) {
	cases := []struct {
		name string
		s    Settings
		ok   bool
	}{
		{"default", DefaultSettings(), true},
		{"zero limits", Settings{WarnPercent: 0, Over: OverBlockAll}, true},
		{"full warn", Settings{WarnPercent: 100, Over: OverSkipBackground}, true},
		{"empty over", Settings{WarnPercent: 80}, false},
		{"empty", Settings{}, false},
		{"negative tokens", Settings{MonthlyTokens: -1, WarnPercent: 80, Over: OverSkipBackground}, false},
		{"negative cost", Settings{MonthlyCost: -0.01, WarnPercent: 80, Over: OverSkipBackground}, false},
		{"warn above", Settings{WarnPercent: 101, Over: OverSkipBackground}, false},
		{"warn below", Settings{WarnPercent: -1, Over: OverSkipBackground}, false},
		{"over unknown", Settings{WarnPercent: 80, Over: OverBlockAll + "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateSettings(c.s)
			if c.ok && err != nil {
				t.Fatalf("应当合法: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("%+v 应当被拒绝", c.s)
			}
		})
	}
}

// 金额上限必须至少能表示成一个纳单位：舍入成 0 时金额检查整个被跳过，预算显示
// 已启用却永不拦截。5e-10 恰好舍入成 1，可接受；1e-10 舍入成 0，必须拒绝。
// 上限也不能大到换不成 int64：溢出方向不确定，一样等于没设。
func TestValidateSettingsCostBoundary(t *testing.T) {
	cases := []struct {
		name string
		cost float64
		ok   bool
		want error // 非 nil 时还要求命中这个哨兵错误（调用方靠它翻文案）
	}{
		{"一纳单位", 1e-9, true, nil},
		{"半个纳单位舍入成 1", 5e-10, true, nil},
		{"不足一纳单位", 1e-10, false, ErrCostTooSmall},
		{"更小", 1e-12, false, ErrCostTooSmall},
		{"过大", 1e10, false, ErrCostTooLarge},
		{"Inf", math.Inf(1), false, ErrCostTooLarge},
		{"NaN", math.NaN(), false, nil}, // 非有限数，不是边界太小/太大的问题
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := DefaultSettings()
			s.MonthlyCost = c.cost
			err := ValidateSettings(s)
			if c.ok && err != nil {
				t.Fatalf("%v 应当合法: %v", c.cost, err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("%v 应当被拒绝", c.cost)
				}
				if c.want != nil && !errors.Is(err, c.want) {
					t.Fatalf("%v 应返回 %v，得到: %v", c.cost, c.want, err)
				}
			}
		})
	}
}

// 太小或太大的金额上限保存不进去，磁盘上那份也不许动：写了一半的预算比没写更糟。
func TestSaveSettingsRejectsCostBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	body := `{"version": 1, "monthly_cost": 5, "over": "block_all"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cost := range []float64{1e-10, 1e10} {
		s := DefaultSettings()
		s.MonthlyCost = cost
		if err := SaveSettings(path, s); err == nil {
			t.Fatalf("%v 应当被拒绝", cost)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != body {
			t.Fatalf("%v 被拒后不该改写文件: %q", cost, raw)
		}
	}
}

// 手写的文件里出现 1e-10：读不出来就按默认值（不设限）+ 错误返回，绝不给出
// 一份「看起来设了限、实际舍入成 0」的配置。
func TestLoadSettingsRejectsTinyCost(t *testing.T) {
	got, err := LoadSettings(writeSettingsFile(t, `{"monthly_cost": 1e-10}`))
	if err == nil {
		t.Fatalf("1e-10 应当报错，got %+v", got)
	}
	if !errors.Is(err, ErrCostTooSmall) {
		t.Fatalf("应返回 ErrCostTooSmall: %v", err)
	}
	if got != DefaultSettings() {
		t.Fatalf("出错时应返回默认值，got %+v", got)
	}
}

// 手写文件里拼错一个键，静默忽略就等于「预算其实没设上」。必须报错。
func TestLoadSettingsRejectsUnknownField(t *testing.T) {
	cases := map[string]string{
		"拼错的键":  `{"monthly_token": 1000}`,
		"大小写不符": `{"MonthlyTokens": 1000}`,
		"多出来的键": `{"monthly_tokens": 1, "budget": 10}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := LoadSettings(writeSettingsFile(t, body))
			if err == nil {
				t.Fatalf("未知字段应当报错，got %+v", got)
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("错误应说明是未知字段: %v", err)
			}
			if got != DefaultSettings() {
				t.Fatalf("出错时应返回默认值，got %+v", got)
			}
		})
	}
}

// null 与「没写」解开之后长得一样，意思却可能相反（清空限额 vs 忘了填）。
func TestLoadSettingsRejectsNull(t *testing.T) {
	for _, key := range []string{"version", "monthly_tokens", "monthly_cost", "currency", "warn_percent", "over"} {
		t.Run(key, func(t *testing.T) {
			got, err := LoadSettings(writeSettingsFile(t, `{"`+key+`": null}`))
			if err == nil {
				t.Fatalf("%s 为 null 应当报错，got %+v", key, got)
			}
			if !strings.Contains(err.Error(), "must not be null") {
				t.Fatalf("错误应说明不能为 null: %v", err)
			}
			if got != DefaultSettings() {
				t.Fatalf("出错时应返回默认值，got %+v", got)
			}
		})
	}
}

// 记事本存出来的文件带 BOM，json 见到它会当成非法字符，整份设置就静默退回默认。
func TestLoadSettingsAcceptsBOM(t *testing.T) {
	path := writeSettingsFile(t, "\xef\xbb\xbf"+`{"monthly_tokens": 42}`)
	got, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("带 BOM 的文件不该报错: %v", err)
	}
	want := DefaultSettings()
	want.MonthlyTokens = 42
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// 手工编辑的文件多为 CRLF。
func TestLoadSettingsAcceptsCRLF(t *testing.T) {
	body := "{\r\n  \"monthly_tokens\": 123,\r\n  \"over\": \"block_all\"\r\n}\r\n"
	got, err := LoadSettings(writeSettingsFile(t, body))
	if err != nil {
		t.Fatalf("CRLF 文件不该报错: %v", err)
	}
	want := DefaultSettings()
	want.MonthlyTokens = 123
	want.Over = OverBlockAll
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// 版本比本程序新：不猜也不写回，报错等用户升级。
func TestLoadSettingsRejectsNewerVersion(t *testing.T) {
	body := `{"version": 2, "monthly_tokens": 10}`
	path := writeSettingsFile(t, body)
	got, err := LoadSettings(path)
	if err == nil {
		t.Fatalf("version 2 应当报错，got %+v", got)
	}
	if !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("错误应带上读到的版本: %v", err)
	}
	// 调用方要能把它与「文件坏了」区分开：前者只能等升级，后者可以覆盖。
	var tooNew *SettingsTooNewError
	if !errors.As(err, &tooNew) || tooNew.Found != 2 || tooNew.Supported != settingsVersion {
		t.Fatalf("错误应是 *SettingsTooNewError{2, %d}: %v", settingsVersion, err)
	}
	if got != DefaultSettings() {
		t.Fatalf("出错时应返回默认值，got %+v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Fatalf("读到新版本时不该改写文件: %q", raw)
	}
}

// 更新的版本必然带本程序不认识的字段：那不能算「文件坏了」，否则调用方会按默认值
// 覆盖掉它——正是版本检查要防的事。
func TestLoadSettingsNewerVersionWithUnknownField(t *testing.T) {
	body := `{"version": 2, "monthly_tokens": 10, "future_limit": 5}`
	got, err := LoadSettings(writeSettingsFile(t, body))
	var tooNew *SettingsTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("应当先认版本、再挑未知键，got %+v / %v", got, err)
	}
	if got != DefaultSettings() {
		t.Fatalf("出错时应返回默认值，got %+v", got)
	}
}

// 写盘前先看现有文件的版本：更新的版本写的文件整份覆盖会抹掉不认识的字段，不许写。
func TestSaveSettingsRefusesNewerVersion(t *testing.T) {
	body := `{"version": 2, "monthly_tokens": 10, "future_limit": 5}`
	path := writeSettingsFile(t, body)
	err := SaveSettings(path, Settings{MonthlyTokens: 99, WarnPercent: 50})
	var tooNew *SettingsTooNewError
	if !errors.As(err, &tooNew) || tooNew.Found != 2 || tooNew.Supported != settingsVersion {
		t.Fatalf("应当返回 *SettingsTooNewError，got %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 一个字节都不动：版本、限额、新字段都还在。
	if string(raw) != body {
		t.Fatalf("不许改写更新的版本写的文件:\n%s", raw)
	}
}

// 文件坏了不是「更新的版本」：用户多半正想用一份新配置修好它，照常覆盖。
func TestSaveSettingsOverwritesBrokenFile(t *testing.T) {
	path := writeSettingsFile(t, "{ 不是 JSON")
	if err := SaveSettings(path, Settings{MonthlyTokens: 5}); err != nil {
		t.Fatalf("坏文件应当允许覆盖: %v", err)
	}
	got, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if got.MonthlyTokens != 5 {
		t.Fatalf("MonthlyTokens = %d, want 5", got.MonthlyTokens)
	}
}

// 手写的文件常常不写 version：按当前版本处理。
func TestLoadSettingsMissingVersionIsCurrent(t *testing.T) {
	got, err := LoadSettings(writeSettingsFile(t, `{"monthly_tokens": 7}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("Version = %d, want 1", got.Version)
	}
}

// 币种统一成大写：价格表里写 "cny"、预算里写 "CNY" 说的是同一个币种，不该因为
// 大小写不同各算一桶，让「这个月花了多少」显示成 0。
func TestSettingsCurrencyUppercased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := SaveSettings(path, Settings{Currency: " cny ", Over: OverBlockAll}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Currency != "CNY" {
		t.Fatalf("Currency = %q, want CNY", got.Currency)
	}
	// 手写的小写币种在读入时同样归一化
	got, err = LoadSettings(writeSettingsFile(t, `{"currency": "usd"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Currency != "USD" {
		t.Fatalf("Currency = %q, want USD", got.Currency)
	}
}

// 写出的文件是给人看的：LF 结尾、version 为当前版本。
func TestSaveSettingsFileShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := SaveSettings(path, Settings{MonthlyTokens: 5}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("文件应以 \\n 结尾: %q", raw)
	}
	if bytes.Contains(raw, []byte("\r")) {
		t.Fatalf("不该写成 CRLF: %q", raw)
	}
	if !bytes.Contains(raw, []byte(`"version": 1`)) {
		t.Fatalf("version 应为 1: %s", raw)
	}
}
