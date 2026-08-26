package health

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wen/internal/prompt"
)

// file 是一个可见域落盘的全部内容。
type file struct {
	Seq        int         `json:"seq"`
	Conditions []Condition `json:"conditions"`
	// 上一次痊愈的时刻与那条状况的名字：冷却与「刚病好」的提示都从这里算。
	LastRecovered time.Time `json:"last_recovered"`
	LastName      string    `json:"last_name,omitempty"`
}

// Limits 是登记时的硬约束，来自插件配置。
type Limits struct {
	Cooldown      time.Duration
	MaxConditions int
	MaxPeak       int
}

// Snapshot 是一个可见域折算到某一时刻的全貌。
type Snapshot struct {
	Active        []View // 还没走完病程的状况（含苗头），按登记顺序
	LastRecovered time.Time
	LastName      string
}

// Update 是一次修改要改的项；nil 表示不动。
type Update struct {
	Care      *string
	Severity  *string
	Recovered bool
}

// Store 管理一个可见域的身体状况。单个 JSON 文件，每次操作重新读取不做缓存。
type Store struct {
	mu sync.Mutex
	// lang 是进程语言。这个库的错误与回执片段会经工具返回给模型，跟着语言走。
	lang string
	dir  string
	path string
}

// NewStore 建立指向 <dir>/health.json 的库。
func NewStore(lang, dir string) *Store {
	return &Store{lang: lang, dir: dir, path: filepath.Join(dir, "health.json")}
}

// Current 返回折算到 now 的全貌。读取路径只折算不回写：读一次写一次盘没有必要，
// 已走完病程的状况留到下一次写入时再清。
func (s *Store) Current(now time.Time) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return Snapshot{}, err
	}
	settle(&f, now)
	snap := Snapshot{LastRecovered: f.LastRecovered, LastName: f.LastName}
	for _, c := range f.Conditions {
		snap.Active = append(snap.Active, c.view(now))
	}
	return snap, nil
}

// Add 登记一条新状况。冷却、同时数量、峰值封顶三条硬约束在这里把关；被拒时
// 错误文本写明规则，否则模型只会换个名字再试一次。返回的 Condition 已按上限收过。
func (s *Store) Add(name, severity string, onset time.Time, days int, care string, now time.Time, lim Limits) (Condition, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return Condition{}, fmt.Errorf("%s", prompt.T(s.lang, "prompt.health.result.name_empty", "状况名不能为空"))
	}
	if n := len([]rune(name)); n > maxNameRunes {
		return Condition{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.name_long",
			"状况名过长（%d 字），最多 %d 字"), n, maxNameRunes)
	}
	peak, ok := peakFor(severity)
	if !ok {
		return Condition{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.bad_severity",
			"严重度只能是「%s」「%s」「%s」之一"),
			sevMild.For(s.lang), sevModerate.For(s.lang), sevSevere.For(s.lang))
	}
	// 落盘的是中文规范值：英文界面下模型报的是 "tough it out"，存进去的仍是「硬扛」，
	// 于是换一次语言不会让同一个字段有两种写法，已经存下的记录也不必迁移。
	care, okCare := careKinds.Canonical(care)
	if !okCare {
		return Condition{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.bad_care",
			"处理方式只能是「%s」「%s」「%s」之一"), careKinds.For(s.lang, careTough),
			careKinds.For(s.lang, careMeds), careKinds.For(s.lang, careDoctor))
	}
	if days < minDays || days > maxDays {
		return Condition{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.bad_days",
			"病程天数要在 %d 到 %d 之间：拖过两周的就不是日常小恙了"), minDays, maxDays)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return Condition{}, err
	}
	settle(&f, now)

	if !f.LastRecovered.IsZero() && lim.Cooldown > 0 {
		if since := now.Sub(f.LastRecovered); since >= 0 && since < lim.Cooldown {
			return Condition{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.cooldown",
				"上次「%s」%s才好，按规则痊愈后 %d 天内不再添新状况（还差%s）。"+
					"真要演身体不适，把它当作还没完全恢复的余波，不另记一条"),
				f.LastName, agoText(s.lang, since), int(lim.Cooldown.Hours()/24),
				leftText(s.lang, lim.Cooldown-since))
		}
	}
	if lim.MaxConditions > 0 && len(f.Conditions) >= lim.MaxConditions {
		return Condition{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.too_many",
			"同时最多记 %d 个状况，现在已有：%s。"+
				"先用 update_condition 把其中一个标记痊愈，或把新的不适并进已有的状况里"),
			lim.MaxConditions, strings.Join(names(f.Conditions), sepFor(s.lang)))
	}
	if i := indexOf(f.Conditions, name); i >= 0 {
		return Condition{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.duplicate",
			"已有同名状况「%s」，要改它用 update_condition"), f.Conditions[i].Name)
	}

	if lim.MaxPeak > 0 {
		peak = min(peak, lim.MaxPeak)
	}
	if onset.Before(now) {
		onset = now
	}
	f.Seq++
	c := Condition{
		ID:         fmt.Sprintf("%d", f.Seq),
		Name:       name,
		Peak:       peak,
		Onset:      onset,
		Days:       days,
		Care:       care,
		ProgressAt: onset,
		Cued:       !onset.After(now), // 此刻就发作的不需要开口理由：模型正在这一轮里
	}
	f.Conditions = append(f.Conditions, c)
	if err := s.save(f); err != nil {
		return Condition{}, err
	}
	return c, nil
}

// Apply 修改一条状况。name 为空且只有一条状况时默认指它。
// 返回修改后的状况与一组「改了什么」的说明。严重度同样受峰值封顶：登记时封住、
// 改的时候放开，上限就形同虚设。
func (s *Store) Apply(name string, u Update, now time.Time, lim Limits) (Condition, []string, error) {
	var care string
	if u.Care != nil {
		var ok bool
		if care, ok = careKinds.Canonical(*u.Care); !ok {
			return Condition{}, nil, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.bad_care",
				"处理方式只能是「%s」「%s」「%s」之一"), careKinds.For(s.lang, careTough),
				careKinds.For(s.lang, careMeds), careKinds.For(s.lang, careDoctor))
		}
	}
	var newPeak int
	capped := false
	if u.Severity != nil {
		p, ok := peakFor(*u.Severity)
		if !ok {
			return Condition{}, nil, fmt.Errorf(prompt.T(s.lang, "prompt.health.result.bad_severity",
				"严重度只能是「%s」「%s」「%s」之一"),
				sevMild.For(s.lang), sevModerate.For(s.lang), sevSevere.For(s.lang))
		}
		newPeak = p
		if lim.MaxPeak > 0 && newPeak > lim.MaxPeak {
			newPeak, capped = lim.MaxPeak, true
		}
	}
	if u.Care == nil && u.Severity == nil && !u.Recovered {
		return Condition{}, nil, fmt.Errorf("%s", prompt.T(s.lang, "prompt.health.result.nothing_to_change",
			"没有要改的项：处理方式、严重度、痊愈至少给一项"))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return Condition{}, nil, err
	}
	settle(&f, now)

	i, err := pick(s.lang, f.Conditions, name)
	if err != nil {
		return Condition{}, nil, err
	}
	c := f.Conditions[i]
	var changes []string

	if u.Recovered {
		f.Conditions = append(f.Conditions[:i], f.Conditions[i+1:]...)
		f.LastRecovered, f.LastName = now, c.Name
		if err := s.save(f); err != nil {
			return Condition{}, nil, err
		}
		c.Progress, c.ProgressAt = 1, now
		return c, []string{prompt.T(s.lang, "prompt.health.change.recovered", "已痊愈")}, nil
	}

	// 先把进度折算到此刻，之后的改动都从这里起算。还没发作的不折算：基准仍是发作时刻。
	if now.After(c.ProgressAt) {
		c.Progress, c.ProgressAt = c.progressAt(now), now
	}
	if care != "" && care != c.Care {
		changes = append(changes, prompt.Tf(s.lang, "prompt.health.change.care", "处理方式 %s→%s",
			careKinds.For(s.lang, c.Care), careKinds.For(s.lang, care)))
		c.Care = care
	}
	if u.Severity != nil {
		cur := severityAt(c.Peak, c.Progress)
		switch {
		case now.Before(c.Onset):
			// 还没发作：改的是预计会有多重
			c.Peak = newPeak
		case newPeak >= cur:
			// 加重：从此刻起以新的峰值重新起算好转段
			c.Peak, c.Progress = newPeak, riseEnd
		default:
			// 好转：峰值不变，把进度拨到严重度恰好等于新档的位置
			c.Progress = 1 - float64(newPeak)/float64(c.Peak)*(1-riseEnd)
		}
		changes = append(changes, prompt.Tf(s.lang, "prompt.health.change.severity",
			"严重度改为%s", band(s.lang, newPeak)))
		// 拦截生效时要把规则告诉模型，否则它只会换个说法再报一次
		if capped {
			changes = append(changes, prompt.Tf(s.lang, "prompt.health.change.capped",
				"按上限收成「%s」（原报「%s」），只演绎日常小恙", band(s.lang, newPeak), *u.Severity))
		}
	}
	f.Conditions[i] = c
	if err := s.save(f); err != nil {
		return Condition{}, nil, err
	}
	return c, changes, nil
}

// MarkCued 记下某条状况的发作理由已经投递（或已错过）。
func (s *Store) MarkCued(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	for i := range f.Conditions {
		if f.Conditions[i].ID == id {
			if f.Conditions[i].Cued {
				return nil
			}
			f.Conditions[i].Cued = true
			return s.save(f)
		}
	}
	return nil
}

// Clear 抹掉本库的全部记录，返回被清掉的状况 id 供调用方撤回开口理由。
// 文件不存在时不算错误。
func (s *Store) Clear() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(f.Conditions))
	for _, c := range f.Conditions {
		ids = append(ids, c.ID)
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf(prompt.T(s.lang, "prompt.health.store.clear_failed", "清除身体状况失败: %w"), err)
	}
	return ids, nil
}

// settle 把已走完病程的状况折进「上次痊愈」并移出列表。调用方持锁，且只在写入
// 路径上落盘。
func settle(f *file, now time.Time) {
	kept := f.Conditions[:0]
	for _, c := range f.Conditions {
		if c.progressAt(now) >= 1 {
			if at := c.recoveryAt(); at.After(f.LastRecovered) {
				f.LastRecovered, f.LastName = at, c.Name
			}
			continue
		}
		kept = append(kept, c)
	}
	f.Conditions = kept
}

// pick 按名字找状况；名字为空且只有一条时默认指它。
func pick(lang string, cs []Condition, name string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		switch len(cs) {
		case 0:
			return -1, fmt.Errorf("%s", prompt.T(lang, "prompt.health.result.none", "当前没有记下的身体状况"))
		case 1:
			return 0, nil
		}
		return -1, fmt.Errorf(prompt.T(lang, "prompt.health.result.ambiguous",
			"有多个状况，要指明改哪个：%s"), strings.Join(names(cs), sepFor(lang)))
	}
	if i := indexOf(cs, name); i >= 0 {
		return i, nil
	}
	if len(cs) == 0 {
		return -1, fmt.Errorf("%s", prompt.T(lang, "prompt.health.result.none", "当前没有记下的身体状况"))
	}
	return -1, fmt.Errorf(prompt.T(lang, "prompt.health.result.no_such",
		"没有叫「%s」的状况，现有：%s"), name, strings.Join(names(cs), sepFor(lang)))
}

func indexOf(cs []Condition, name string) int {
	for i, c := range cs {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

func names(cs []Condition) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// sepFor 是候选名之间的分隔。
func sepFor(lang string) string { return prompt.T(lang, "prompt.health.sep", "、") }

// agoText 把「多久以前」说成话。不满一小时的说「刚刚」，其余走全程序共用的时长口径。
func agoText(lang string, d time.Duration) string {
	if d < time.Hour {
		return prompt.T(lang, "prompt.health.ago.just_now", "刚刚")
	}
	return prompt.Tf(lang, "prompt.health.ago.before", "%s前", prompt.Duration(lang, d))
}

// leftText 把「还差多久」说成话。这里向上取整（还差一点也要说成「约 1 天」），
// 与 prompt.Duration 的截断口径相反，故不共用。
func leftText(lang string, d time.Duration) string {
	switch h := d.Hours(); {
	case h < 1:
		return prompt.T(lang, "prompt.health.left.within_hour", "不到一小时")
	case h < 24:
		return prompt.Tf(lang, "prompt.health.left.hours", "约 %d 小时", int(h)+1)
	default:
		return prompt.Tf(lang, "prompt.health.left.days", "约 %d 天", int(h/24)+1)
	}
}

func (s *Store) load() (file, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return file{}, nil
		}
		return file{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.store.read_failed", "读取身体状况失败: %w"), err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return file{}, fmt.Errorf(prompt.T(s.lang, "prompt.health.store.corrupt", "身体状况记录文件损坏: %w"), err)
	}
	for i := range f.Conditions {
		c := &f.Conditions[i]
		c.Peak = min(max(c.Peak, 1), 100) // 手改过的文件不该把量程撑破
		c.Days = min(max(c.Days, minDays), maxDays)
		if !validCare(c.Care) {
			c.Care = careTough
		}
		if c.ProgressAt.IsZero() {
			c.ProgressAt = c.Onset
		}
	}
	return f, nil
}

// save 原子写回，权限 0600：身体状况属于对话内容的一部分。
func (s *Store) save(f file) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.health.store.save_failed", "保存身体状况失败: %w"), err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.health.store.mkdir_failed", "创建身体状况目录失败: %w"), err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf(prompt.T(s.lang, "prompt.health.store.save_failed", "保存身体状况失败: %w"), err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf(prompt.T(s.lang, "prompt.health.store.save_failed", "保存身体状况失败: %w"), err)
	}
	return nil
}
