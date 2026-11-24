package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"wen/internal/atomicfile"
)

// 预算耗尽后的两种处置，除此之外一律不认：多一个拼错的取值就会变成第三种
// 没人实现过的行为，而它只在用户手改配置之后才会出现。
const (
	OverSkipBackground = "skip_background"
	OverBlockAll       = "block_all"
)

// settingsVersion 是本程序认识的 usage.json 版本。读到更新的版本只报错、不写回：
// 那份文件里可能有本程序不认识的字段，按默认值重写会把用户的预算改掉。
const settingsVersion = 1

// ErrCostTooSmall 与 ErrCostTooLarge 是金额上限的两条边界错误。
//
// 单列出来是为了让调用方（Web 设置保存）把它们翻成用户看得懂的文案：其余校验错误
// 按原话报就够了，这两条说的是「填了但等于没设」，得解释清楚。costUnits 的精度是
// 一纳单位（见 budget.go），低于它的正数会被舍入成 0，金额检查于是整个被跳过——
// 预算显示已启用却永不拦截。
var (
	ErrCostTooSmall = errors.New("usage: monthly_cost is too small to represent")
	ErrCostTooLarge = errors.New("usage: monthly_cost is too large to represent")
)

// SettingsTooNewError 表示磁盘上的 usage.json 是更新的版本写的。单列一个类型是为了
// 让调用方能把它与「文件坏了」区分开：后者可以当默认值覆盖掉（用户正在修），前者
// 必须原样留着等用户升级——覆盖一次就会把新版本才有的预算字段抹平。
type SettingsTooNewError struct{ Found, Supported int }

func (e *SettingsTooNewError) Error() string {
	return fmt.Sprintf("usage settings: version %d is newer than %d, upgrade the program first",
		e.Found, e.Supported)
}

// Settings 是账本的预算设置。限额为 0 表示不设限；币种是金额的记账单位，价格由
// 用户自己填在价格表里，只有该币种的金额参与 MonthlyCost 的判定。
type Settings struct {
	Version       int     `json:"version"`
	MonthlyTokens int64   `json:"monthly_tokens"`
	MonthlyCost   float64 `json:"monthly_cost"` // 币种见 Currency；没有价格表时这一项不生效
	Currency      string  `json:"currency"`     // 记账币种，存盘时为大写
	WarnPercent   int     `json:"warn_percent"`
	Over          string  `json:"over"` // skip_background | block_all
}

// settingsFields 是 usage.json 认得的键。手写文件里拼错的键必须报错：静默忽略会
// 让人以为「限额已经设上了」，实际跑的却是不设限的默认值——预算失效最坏的形态。
var settingsFields = map[string]bool{
	"version":        true,
	"monthly_tokens": true,
	"monthly_cost":   true,
	"currency":       true,
	"warn_percent":   true,
	"over":           true,
}

// DefaultSettings 是不设预算的那一档：限额为 0（等于不设限），只留提醒阈值与
// 币种这类总得有值的字段。
func DefaultSettings() Settings {
	return Settings{
		Version:     settingsVersion,
		Currency:    "CNY",
		WarnPercent: 80,
		Over:        OverSkipBackground,
	}
}

// LoadSettings 读取 <配置目录>/usage.json。文件不存在返回默认值，不算错误；
// JSON 损坏、带了不认识的键或校验不过时返回默认值 + error，让调用方决定是报错
// 还是按默认跑——但绝不会拿到半份配置。
func LoadSettings(path string) (Settings, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultSettings(), nil
		}
		return DefaultSettings(), err
	}
	return parseSettings(raw)
}

// parseSettings 解析并校验一份 usage.json。任何错误都配默认值返回。
func parseSettings(raw []byte) (Settings, error) {
	// 手工编辑的文件常带 UTF-8 BOM（记事本就这么存），json 会把它当成非法字符，
	// 于是整份设置静默退回默认值。与 memory 的 normalizeText 同样先去掉。
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	// 先按键收一遍原始值：Json 解进结构体时未知键会被丢掉，而这里恰恰要拦住它们。
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return DefaultSettings(), fmt.Errorf("parse usage settings: %w", err)
	}
	if fields == nil {
		return DefaultSettings(), fmt.Errorf("parse usage settings: want a JSON object")
	}
	// 先认版本，再挑未知键：更新的版本本来就会带本程序不认识的字段，先按「未知键」
	// 报错会把它降级成「文件坏了」，调用方于是把它当默认值覆盖掉——恰好是版本检查
	// 要防的那件事。version 本身能读就先判，读不动再交给后面的结构体解码报错。
	if rawVer, ok := fields["version"]; ok && string(bytes.TrimSpace(rawVer)) != "null" {
		var v int
		if err := json.Unmarshal(rawVer, &v); err == nil && v > settingsVersion {
			return DefaultSettings(), &SettingsTooNewError{Found: v, Supported: settingsVersion}
		}
	}
	for k, v := range fields {
		if !settingsFields[k] {
			return DefaultSettings(), fmt.Errorf("usage settings: unknown field %q", k)
		}
		// null 与「没写」解开之后长得一样，意思却可能相反（清空限额 vs 忘了填）。
		// 手写文件里出现 null 多半是编辑器或模板留下的，报错比猜安全；字符串字段
		// 也一并不收，少一种歧义。
		if string(bytes.TrimSpace(v)) == "null" {
			return DefaultSettings(), fmt.Errorf("usage settings: %q must not be null", k)
		}
	}

	// 从默认值起解：文件漏了哪一项（手写时常见）就沿用默认值，而不是留下零值——
	// 零值的 over 是空串，落进校验只会让人无从下手。
	s := DefaultSettings()
	if err := json.Unmarshal(raw, &s); err != nil {
		return DefaultSettings(), fmt.Errorf("parse usage settings: %w", err)
	}
	switch {
	case s.Version > settingsVersion:
		// 上面的版本探针已经拦过；这里留着兜底，口径保持一致。
		return DefaultSettings(), &SettingsTooNewError{Found: s.Version, Supported: settingsVersion}
	case s.Version == 0:
		s.Version = settingsVersion // 手写的文件常常不写 version
	}
	s = normalize(s)
	if err := ValidateSettings(s); err != nil {
		return DefaultSettings(), err
	}
	return s, nil
}

// SaveSettings 归一化并校验后整份写回，0600（里面是用户自己的预算）。
//
// 写之前先看一眼现有文件的版本：磁盘上那份若是更新的版本写的，就不写——整份覆盖会
// 把本程序不认识的字段（新版本才有的预算项）一并抹掉，而用户看到的是「保存成功」。
// 文件不存在或不是 JSON 时照常覆盖：用户多半正想用一份干净的配置修好它。
func SaveSettings(path string, s Settings) error {
	s = normalize(s)
	s.Version = settingsVersion // 写出的永远是当前版本
	if err := ValidateSettings(s); err != nil {
		return err
	}
	found, err := probeSettingsVersion(path)
	if err != nil {
		return err
	}
	if found > settingsVersion {
		return &SettingsTooNewError{Found: found, Supported: settingsVersion}
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create usage settings dir: %w", err)
	}
	return atomicfile.WriteFile(path, append(raw, '\n'), 0o600)
}

// probeSettingsVersion 只问一件事：现有文件写的 version 是多少。它刻意不复用
// parseSettings——那份解析会因为未知键、null、校验不过而报错，而这里关心的恰恰是
// 「一份本程序看不懂的文件里写着更新的版本」这种情况，不能被别的错误遮住。
// 文件不存在或不是 JSON 都返回 0（当作可覆盖）。
func probeSettingsVersion(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return 0, nil // 坏了：允许用一份新配置覆盖
	}
	return probe.Version, nil
}

// ValidateSettings 校验字段取值。零值不算合法（over 是空串），因为 DefaultSettings
// 是唯一该被当成「没填」的来源。
//
// 金额上限必须至少能表示成一个纳单位、又不超过 int64 能装下的范围：前者舍入成 0 会
// 让金额判定整个被跳过（界面上显示已启用，实际永不拦截），后者换不成纳单位、溢出的
// 方向不确定。两边都由状态里的 LimitTokens/LimitCost 之外的行为体现，用户看不出来，
// 所以只能在保存时拒绝。
func ValidateSettings(s Settings) error {
	if s.MonthlyTokens < 0 {
		return fmt.Errorf("usage: monthly_tokens must not be negative")
	}
	if math.IsNaN(s.MonthlyCost) {
		return fmt.Errorf("usage: monthly_cost must be a finite number")
	}
	if s.MonthlyCost < 0 {
		return fmt.Errorf("usage: monthly_cost must not be negative")
	}
	if s.MonthlyCost > 0 {
		// 先挡过大的：超出 int64 能表示的范围时，costUnits 的溢出方向不确定。
		if limit := float64(math.MaxInt64) / costUnitsPerUnit; math.IsInf(s.MonthlyCost, 0) || s.MonthlyCost >= limit {
			return fmt.Errorf("%w: at most %g", ErrCostTooLarge, limit)
		}
		// 5e-10 恰好舍入成 1 纳单位，可接受；1e-10 舍入成 0，等于没设上限。
		if costUnits(s.MonthlyCost) < 1 {
			return fmt.Errorf("%w: at least %g", ErrCostTooSmall, 1.0/costUnitsPerUnit)
		}
	}
	if s.WarnPercent < 0 || s.WarnPercent > 100 {
		return fmt.Errorf("usage: warn_percent must be between 0 and 100")
	}
	switch s.Over {
	case OverSkipBackground, OverBlockAll:
		return nil
	}
	return fmt.Errorf("usage: unknown over policy %q", s.Over)
}

// normalize 补上「没填」的字段。空串按未设置处理，与插件配置的空串语义一致：
// 界面清空一个输入框、手写文件时少写一项，提交的都是空串。
//
// 币种统一成大写：它是金额的分桶键（见 CostTotals），价格表里写 "cny"、预算里写
// "CNY" 说的是同一个币种，不该因此各算一桶、让已用金额显示为 0。
func normalize(s Settings) Settings {
	s.Currency = strings.ToUpper(strings.TrimSpace(s.Currency))
	if s.Currency == "" {
		s.Currency = "CNY"
	}
	s.Over = strings.TrimSpace(s.Over)
	if s.Over == "" {
		s.Over = OverSkipBackground
	}
	return s
}
