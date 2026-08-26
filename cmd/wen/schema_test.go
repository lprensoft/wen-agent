package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"wen/internal/config"
	"wen/internal/i18n"
	"wen/internal/plugin"
)

// stubSessions 让依赖会话查询的插件能起来。这里要测的是 Schema 的文本，
// 不是它们的业务，所以答什么都行，只要不是「不可用」。
type stubSessions struct{}

func (stubSessions) LastActive() (string, time.Time, error) { return "", time.Time{}, nil }
func (stubSessions) LastInteraction() (time.Time, error)    { return time.Time{}, nil }
func (stubSessions) Exists(string) bool                     { return false }

// TestToolSchemasAreValidJSONInEveryLanguage 盯住一处静默故障。
//
// 工具的参数 Schema 是整段手写的 JSON，翻译单位就是整段。译者少一个引号、漏掉一个
// 占位符，出来的仍是一个字符串，程序照常启动，只有模型那边收到一份解不开的 Schema
// ——而那表现为「这个工具怎么老是调不对」，不表现为错误。
//
// 这里只解析，不比对结构：Schema 的形状本来就是各插件自己的事。
func TestToolSchemasAreValidJSONInEveryLanguage(t *testing.T) {
	for _, l := range i18n.Languages() {
		cfg := &config.Config{BaseDir: t.TempDir()}
		// 少一个能力就会有一批插件起不来，而依赖它们的又会连带被关掉——
		// 那样这条测试会「通过」，只是什么都没查。
		ictx := plugin.InitContext{
			Workdir:    t.TempDir(),
			Lang:       l.Code,
			SessionDir: t.TempDir(),
			Sessions:   stubSessions{},
			RunTurn:    func(context.Context, string, string) (string, error) { return "", nil },
			NewSession: func() (string, error) { return "s1", nil },
			Compact:    func(context.Context, string) error { return nil },
			Notice:     func(context.Context, string, string) error { return nil },
		}
		m := buildPlugins(cfg, ictx, nil)
		m.Resolve()
		// 默认关掉的那几个也要查：它们的 Schema 一样会发给模型，只是要用户先打开。
		// 开不起来的（缺凭证、缺触发词）跳过，这里不是在测启用逻辑。
		for _, st := range m.List() {
			if !st.Enabled {
				_ = m.SetEnabled(st.Name, true)
			}
		}

		tools := m.EnabledTools()
		if len(tools) < 20 {
			t.Fatalf("%s: 只取到 %d 个工具，多半是有一批插件没起来，这条测试就成了空跑",
				l.Code, len(tools))
		}
		for _, tool := range tools {
			var v any
			if err := json.Unmarshal(tool.Schema(), &v); err != nil {
				t.Errorf("%s: 工具 %s 的 Schema 不是合法 JSON: %v\n%s",
					l.Code, tool.Name(), err, tool.Schema())
			}
		}
	}
}
