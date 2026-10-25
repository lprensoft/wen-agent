package bookshelf

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"wen/internal/i18n"
	"wen/internal/plugin"
)

// actionShelf 是设置页上的「看看书架」按钮。书是用户自己往目录里放的文件，而那个目录
// 在配置目录深处，不说清楚没人找得到；顺带报一下每本书有多长、读到哪儿、哪本打不开。
const actionShelf = "shelf"

func (p *Plugin) Actions() []plugin.ActionDef {
	shelf, _ := p.settings()
	// 只放路径本身：Detail 不翻译，说明那一句以冒号收尾，接上正好
	return []plugin.ActionDef{{
		Key:   actionShelf,
		Label: "看看书架",
		Description: "把 .txt 或 .md 文件放进书架目录，一个文件是一本书，文件名就是书名；GBK 与 UTF-16 编码的也认。" +
			"放进去就能读，不必重启。书架目录：",
		Detail: shelf,
	}}
}

// StartAction 同步列一遍书架：只读目录与文件，不调模型，几本书的量不需要后台跑。
func (p *Plugin) StartAction(_ context.Context, key string) error {
	lang := p.langOf()
	if key != actionShelf {
		return plugin.UnknownAction(lang, key)
	}
	shelf, _ := p.settings()
	if shelf == "" {
		return fmt.Errorf("%s", i18n.T(lang, "plugin.bookshelf.not_ready", "书架尚未就绪"))
	}
	st := p.report(lang, shelf)
	p.actMu.Lock()
	p.actState = st
	p.actMu.Unlock()
	return nil
}

func (p *Plugin) report(lang, shelf string) plugin.ActionState {
	var b strings.Builder
	b.WriteString(i18n.Tf(lang, "plugin.bookshelf.report.dir", "书架目录：%s\n\n", shelf))
	books, err := listBooks(shelf)
	if err != nil {
		b.WriteString(i18n.Tf(lang, "plugin.bookshelf.report.unreadable", "读不了这个目录：%v", err))
		return plugin.ActionState{Status: plugin.ActionError, Message: b.String()}
	}
	if len(books) == 0 {
		b.WriteString(i18n.T(lang, "plugin.bookshelf.report.empty", "书架上还没有书。把 .txt 或 .md 文件放进上面这个目录就行。"))
		return plugin.ActionState{Status: plugin.ActionDone, Message: b.String()}
	}

	// 进度报表人格（共享域）那一份：设置页是操作台，不按可见域过滤，但两份并列只会让人困惑
	var progress map[string]*Progress
	if store := p.storeFor(""); store != nil {
		if s, err := store.Load(); err == nil {
			progress = s.Books
		}
	}
	b.WriteString(i18n.Tf(lang, "plugin.bookshelf.report.count", "有 %d 本书：\n", len(books)))
	for _, bk := range books {
		raw, err := os.ReadFile(filepath.Join(shelf, bk.File))
		switch {
		case err != nil:
			b.WriteString(i18n.Tf(lang, "plugin.bookshelf.report.line_bad", "· %s：打不开（%v）\n", bk.Title, err))
			continue
		case len(raw) > maxBookBytes:
			b.WriteString(i18n.Tf(lang, "plugin.bookshelf.report.line_too_big", "· %s：太大了，读不了\n", bk.Title))
			continue
		}
		state := i18n.T(lang, "plugin.bookshelf.report.unread", "还没读")
		if pr := progress[bk.File]; pr != nil {
			switch {
			case !pr.Finished.IsZero():
				state = i18n.T(lang, "plugin.bookshelf.report.finished", "读完了")
			case pr.Offset > 0:
				state = i18n.Tf(lang, "plugin.bookshelf.report.progress", "读到约 %d%%", percent(pr.Offset, pr.Length))
			}
		}
		b.WriteString(i18n.Tf(lang, "plugin.bookshelf.report.line", "· %s：约 %d 字，%s\n",
			bk.Title, runeCount(decodeText(raw)), state))
	}
	return plugin.ActionState{Status: plugin.ActionDone, Message: strings.TrimRight(b.String(), "\n")}
}

func (p *Plugin) ActionState(key string) (plugin.ActionState, error) {
	if key != actionShelf {
		return plugin.ActionState{}, plugin.UnknownAction(p.langOf(), key)
	}
	p.actMu.Lock()
	defer p.actMu.Unlock()
	if p.actState.Status == "" {
		return plugin.ActionState{Status: plugin.ActionIdle}, nil
	}
	return p.actState, nil
}
