package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"wen/internal/i18n"

	"wen/internal/plugin"
)

// 提炼窗口的持久化。
//
// 窗口攒在内存里（理由见 turn.go 的文件头：基于消息序号的水位会被压缩的 Replace
// 冲掉），代价原本记作「进程重启丢缓冲」，并指望压缩那条路径兜住。实际兜不住——
// 上下文窗口是百万级，压缩要到九成才触发，大多数会话一辈子走不到那里。于是重启比
// 「提炼间隔」更频繁的人，定期提炼一次都不会发生，而记忆插件正是靠它积累的。
//
// 所以把缓冲本身落盘。这样既不必引入会被压缩冲掉的序号水位，也不改动窗口的任何
// 既有语义——重启只是接着攒。

const windowFile = "windows.json"

// maxPersistedWindows 限制落盘的窗口数：每个会话 × 每个可见域一个窗口，
// 长期使用下会一直累积。按最后活动时间保留最近的若干个，更早的窗口即便留着，
// 也要等那个会话再次被使用才会被提炼。
const maxPersistedWindows = 20

// persistedTurn 是窗口里的一轮。window 的字段不导出，这里另给一份带标签的形状，
// 而不是给运行时结构加 json 标签——落盘格式该独立于内存表示演化。
type persistedTurn struct {
	User  string `json:"user"`
	Reply string `json:"reply"`
}

type persistedWindow struct {
	Session string          `json:"session"`
	Tag     string          `json:"tag,omitempty"`
	Write   string          `json:"write,omitempty"`
	Read    []string        `json:"read,omitempty"`
	Turns   []persistedTurn `json:"turns"`
	LastEnd time.Time       `json:"last_end"`
}

type persistedState struct {
	Windows []persistedWindow `json:"windows,omitempty"`
	// LastSweep 是上次淡忘清扫的日期。不存的话每次重启都会重扫一遍——
	// 清扫本身是幂等的，但那是一趟无谓的全库遍历。
	LastSweep time.Time `json:"last_sweep,omitempty"`
	// LastTimeline 是上次时间线日切的日期，理由同上（收束还多一次模型调用）。
	LastTimeline time.Time `json:"last_timeline,omitempty"`
}

// dayMarks 是两个按天触发的水位，跟窗口缓冲同一个文件持久化。
type dayMarks struct {
	lastSweep    time.Time
	lastTimeline time.Time
}

func windowPath(dir string) string { return filepath.Join(dir, windowFile) }

// loadWindowState 读回上次的窗口缓冲。文件缺失或损坏时返回空——
// 缓冲丢了只是少提炼一次，不值得让插件起不来。
func loadWindowState(lang, dir string) (map[windowKey]*window, dayMarks) {
	if dir == "" {
		return nil, dayMarks{}
	}
	raw, err := os.ReadFile(windowPath(dir))
	if err != nil {
		return nil, dayMarks{}
	}
	var st persistedState
	if json.Unmarshal(raw, &st) != nil {
		i18n.Logf(lang, "log.memory.window_corrupt", "记忆提炼：窗口缓冲已损坏，忽略并重新开始累计")
		return nil, dayMarks{}
	}

	out := map[windowKey]*window{}
	for _, pw := range st.Windows {
		if pw.Session == "" || len(pw.Turns) == 0 {
			continue
		}
		w := &window{
			scope:   plugin.Scope{Write: pw.Write, Read: pw.Read},
			lastEnd: pw.LastEnd,
		}
		for _, t := range pw.Turns {
			w.turns = append(w.turns, windowTurn{user: t.User, reply: t.Reply})
			w.bytes += len(t.User) + len(t.Reply)
		}
		out[windowKey{session: pw.Session, tag: pw.Tag}] = w
	}
	return out, dayMarks{lastSweep: st.LastSweep, lastTimeline: st.LastTimeline}
}

// saveWindowState 落盘当前缓冲。失败只影响下次启动的连续性，不打断任何事。
func saveWindowState(lang, dir string, windows map[windowKey]*window, marks dayMarks) {
	if dir == "" {
		return
	}
	st := persistedState{LastSweep: marks.lastSweep, LastTimeline: marks.lastTimeline}
	for key, w := range windows {
		if len(w.turns) == 0 {
			continue
		}
		pw := persistedWindow{
			Session: key.session, Tag: key.tag,
			Write: w.scope.Write, Read: w.scope.Read,
			LastEnd: w.lastEnd,
		}
		for _, t := range w.turns {
			pw.Turns = append(pw.Turns, persistedTurn{User: t.user, Reply: t.reply})
		}
		st.Windows = append(st.Windows, pw)
	}
	// 新的在前，超出的丢掉
	sort.Slice(st.Windows, func(i, j int) bool {
		return st.Windows[i].LastEnd.After(st.Windows[j].LastEnd)
	})
	if len(st.Windows) > maxPersistedWindows {
		st.Windows = st.Windows[:maxPersistedWindows]
	}

	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	// 0600：里面是对话原文
	if err := os.WriteFile(windowPath(dir), raw, 0o600); err != nil {
		i18n.Logf(lang, "log.memory.window_write_failed", "记忆提炼：窗口缓冲写入失败：%v", err)
	}
}
