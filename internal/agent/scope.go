package agent

import (
	"wen/internal/llm"
	"wen/internal/plugin"
	"wen/internal/session"
)

// visibleHistory 取出本轮可见域能读到的历史消息。
//
// 标签是按整轮统一分配的（一次 run 内所有落盘消息共用同一个 Write），因此过滤是
// 整轮粒度的：tool_calls 与其结果同去同留，剩下的仍是若干「以 user 开头的完整轮次」
// 的拼接。不变量万一被破坏（外部改动过 jsonl、或进程在一轮中途被杀），
// 由 sanitizeSequence 兜住。
func visibleHistory(stored []session.StoredMessage, sc plugin.Scope) []session.StoredMessage {
	out := make([]session.StoredMessage, 0, len(stored))
	for _, m := range stored {
		if !sc.CanRead(m.Tag) {
			continue
		}
		// 一次性输入（心跳提示词等）只属于它自己那一轮，不进入后续上下文；
		// 它触发的助手回复照常保留（Anthropic 侧连续 assistant 会被合并，安全）
		if m.Kind == session.KindEphemeral {
			continue
		}
		// 会话注记只给人看，任何时候都不进模型上下文
		if m.Kind == session.KindNotice {
			continue
		}
		out = append(out, m)
	}
	return sanitizeSequence(out)
}

// sanitizeSequence 修掉不合法的消息序列，让一段历史仍能安全发给 API：
//  1. 丢弃开头的非 user 消息（首条必须是 user）；
//  2. 丢弃 assistant 中没有对应 tool 结果的 tool_call——调用无法闭合；
//     若整条 assistant 的 tool_calls 都被丢掉且没有正文，整条消息丢弃；
//  3. 丢弃找不到对应 tool_call 的孤立 tool 结果。
//
// 这不只是可见域过滤的需要。run 是先落盘带 tool_calls 的 assistant、再逐个执行
// 工具并落盘结果的；进程若在这两步之间被杀掉，jsonl 里就留下悬挂的 tool_use，
// 该会话此后每次请求都会被 API 以「tool_use 缺少 tool_result」拒绝，而在此之前
// 没有任何修复路径。
func sanitizeSequence(msgs []session.StoredMessage) []session.StoredMessage {
	// 丢弃开头的非 user 消息
	start := 0
	for start < len(msgs) && msgs[start].Role != llm.RoleUser {
		start++
	}
	msgs = msgs[start:]

	// 实际存在的 tool 结果 id
	haveResult := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == llm.RoleTool && m.ToolCallID != "" {
			haveResult[m.ToolCallID] = true
		}
	}

	out := make([]session.StoredMessage, 0, len(msgs))
	keptCall := make(map[string]bool) // 已保留下来的 tool_call id
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				original := len(m.ToolCalls)
				kept := make([]llm.ToolCall, 0, original)
				for _, tc := range m.ToolCalls {
					if haveResult[tc.ID] {
						kept = append(kept, tc)
						keptCall[tc.ID] = true
					}
				}
				if len(kept) == 0 && m.Content == "" {
					continue // 只有悬挂的调用、又没有正文，整条无意义
				}
				m.ToolCalls = kept
				// 原始 Item 里也带着被丢掉的 function_call；历史被修正后不能
				// 再原样回放，否则悬挂调用会绕过这里重新回到请求里。
				if len(kept) != original {
					m.ResponsesItems = nil
					m.ResponsesOrigin = ""
				}
			}
		case llm.RoleTool:
			if !keptCall[m.ToolCallID] {
				continue // 孤立的工具结果
			}
		}
		out = append(out, m)
	}
	return out
}
