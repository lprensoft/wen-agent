package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"wen/internal/i18n"
)

// 检查状态的持久化。
//
// 记的是**上次检查发生的时刻**而不是「还剩多久到下次」：倒计时只活在进程里，
// 重启就归零重算，于是重启比周期更频繁的机器上，一天一次的检查一次也轮不到。
// 存了时刻之后，下次检查由「时刻 + 周期」推算，重启天然接上。
//
// 上次看到的版本也一并存：设置页上那个按钮的文案（「检查更新」还是「更新到 vX
// 并重启」）与状态行都靠它，重启后不该退回「什么都不知道」再等一天。

const stateFile = "state.json"

// maxNotes 是更新说明的留存上限。它只用来在界面上给人看个大概，
// 完整的正文在 Release 页面上。
const maxNotes = 4000

// pendingUpdate 记一次「文件已经换好、等重启生效」的更新。
// 重启之后由 reconcile 按当前版本号认领它（见那里的说明）。
type pendingUpdate struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
}

type state struct {
	LastCheck    time.Time      `json:"last_check,omitzero"`
	Latest       string         `json:"latest_tag,omitempty"`
	LatestAt     time.Time      `json:"latest_published,omitzero"`
	Notes        string         `json:"latest_notes,omitempty"`
	Pending      *pendingUpdate `json:"pending,omitempty"`
	LastUpdate   string         `json:"last_update,omitempty"`
	LastUpdateAt time.Time      `json:"last_update_at,omitzero"`
}

func statePath(dir string) string { return filepath.Join(dir, stateFile) }

// loadState 读回上次的状态。文件缺失或损坏时当作从未检查过——
// 这份状态全部可以重新取得，坏了不值得让插件起不来。
func loadState(lang, dir string) state {
	raw, err := os.ReadFile(statePath(dir))
	if err != nil {
		return state{}
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		i18n.Logf(lang, "log.selfupdate.state_corrupt", "self_update: 状态文件已损坏，按从未检查过处理")
		return state{}
	}
	return st
}

// saveLocked 落盘当前状态。调用方需持有 p.mu。
//
// 写盘在锁内进行：这份状态一天才动一次，锁的代价可以忽略，而换来的是不必去想
// 「两次保存的先后」——后台检查与界面上的操作都会写它。
func (p *Plugin) saveLocked() {
	if p.stateDir == "" {
		return
	}
	raw, err := json.MarshalIndent(p.st, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(p.stateDir, 0o755); err != nil {
		i18n.Logf(p.langOf(), "log.selfupdate.state_dir_failed", "self_update: 创建状态目录失败: %v", err)
		return
	}
	if err := os.WriteFile(statePath(p.stateDir), raw, 0o644); err != nil {
		i18n.Logf(p.langOf(), "log.selfupdate.state_write_failed", "self_update: 状态写入失败: %v", err)
	}
}

// truncateNotes 截断更新说明，避免一份长正文进到状态文件里。
func truncateNotes(s string) string {
	r := []rune(s)
	if len(r) <= maxNotes {
		return s
	}
	return string(r[:maxNotes]) + "…"
}
