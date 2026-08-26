package memory

import (
	"context"
	"fmt"
	"log"
	"strings"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// OnCompact 在会话历史被摘要替换之前，用一次独立的模型调用把其中值得长期保留的
// 结论提炼成记忆条目。
//
// 这里必须是真正的提取而不是提示模型稍后自己去做：自动压缩是无人值守触发的，
// 历史随即被物理删除，而“稍后”这个时机可能永远不会到来——会话可能就此结束。
// 代价是每次压缩多一次模型调用，因此只在压缩这个低频且信息即将丢失的时刻做。
//
// 与定期提炼的分工：这里拿得到完整历史（含工具调用与结果），是信息量最足的一次，
// 也是进程重启丢掉的内存窗口的兜底。原始历史的保全由 session_search 插件的归档
// 负责，两个插件各订阅同一个钩子。
func (p *Plugin) OnCompact(ctx context.Context, ev plugin.CompactEvent) (string, error) {
	s := p.snapshot()
	if s.store == nil || !s.autoExtract || len(ev.History) == 0 {
		return "", nil
	}
	complete := p.completeFunc()
	if complete == nil {
		return "", fmt.Errorf("%s", prompt.T(s.lang, "prompt.memory.err.no_model",
			"当前没有可用的模型，跳过记忆提炼"))
	}

	res, err := p.extractMemories(ctx, s, complete, serializeHistory(s.lang, ev.History, s.maxExtractBytes))
	if err != nil {
		return "", err
	}
	if res.empty() {
		log.Printf("记忆提炼：本次压缩没有找出值得长期保留的内容")
		return "", nil
	}

	names := res.names(s.lang)
	log.Printf("记忆提炼：压缩前新增/修订 %d 条（%s）", len(names), strings.Join(names, "、"))
	if len(names) == 0 {
		return "", nil // 只刷新了提及时间，没有可交代的内容
	}
	return prompt.Tf(s.lang, "prompt.memory.compact.note",
		"（压缩前已从上文提炼并保存 %d 条长期记忆：%s）",
		len(names), strings.Join(names, prompt.T(s.lang, "prompt.memory.name_sep", "、"))), nil
}
