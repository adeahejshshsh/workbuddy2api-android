package upstream

// prompt.keep_recent_rounds：出站前只保留最近 N 轮历史（0 = 关，缺省）。
//
// 「一轮」= 一条 user 消息及其后的 assistant / tool 回复。裁剪点必须落在**整轮边界**
// 上，否则会把 assistant.tool_calls 与配对的 tool 消息切开 —— 上游收到孤立 tool 结果
// 直接 400（tool_call_id 对不上）。system 消息全留：它们是人格/规则，不是历史。
//
// 动机是成本：上下文动辄几十轮，而绝大多数请求只用得上最近几轮。上游实测同一条
// 4 轮请求 prompt_tokens 958 → 38。

import (
	"encoding/json"
	"strings"
)

// trimMessages 按轮裁剪 messages（输入输出都是解码后的通用结构）。
func trimMessages(messages []any, keep int) []any {
	if keep <= 0 || len(messages) == 0 {
		return messages
	}
	// 找出所有 user 消息的下标，作为「轮」的起点。
	var userIdx []int
	for i, m := range messages {
		if roleOf(m) == "user" {
			userIdx = append(userIdx, i)
		}
	}
	if len(userIdx) <= keep {
		return messages
	}
	cut := userIdx[len(userIdx)-keep]
	out := make([]any, 0, len(messages)-cut+4)
	// 保留 cut 之前的所有 system（人格/规则），丢弃其余历史。
	for i := 0; i < cut; i++ {
		if roleOf(messages[i]) == "system" {
			out = append(out, messages[i])
		}
	}
	out = append(out, messages[cut:]...)
	return out
}

// roleOf 取消息的 role（大小写不敏感；非对象/缺字段返回空串）。
func roleOf(m any) string {
	obj, ok := m.(map[string]any)
	if !ok {
		return ""
	}
	r, _ := obj["role"].(string)
	return strings.ToLower(strings.TrimSpace(r))
}

// TrimRounds 对请求体做按轮裁剪；keep<=0、JSON 不合法、messages 不是数组时原样返回。
// 只动 messages，其余字段（model/tools/stream…）逐字节不变。
func TrimRounds(body []byte, keep int) []byte {
	if keep <= 0 || len(body) == 0 {
		return body
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}
	rawMsgs, ok := root["messages"]
	if !ok {
		return body
	}
	var messages []any
	if err := json.Unmarshal(rawMsgs, &messages); err != nil {
		return body
	}
	trimmed := trimMessages(messages, keep)
	if len(trimmed) == len(messages) {
		return body // 没裁就别重编码（保持逐字节一致）
	}
	enc, err := json.Marshal(trimmed)
	if err != nil {
		return body
	}
	root["messages"] = enc
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}
