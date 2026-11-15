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
	"bytes"
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
	// 审查 W4-U1-34 / F-07（P2）：**必须 UseNumber**。
	//
	// 缺陷：json.Unmarshal 把 messages 里所有数字落成 float64，裁剪后重新编码时
	// 工具参数里的 >2^53 整数（雪花 ID、微秒时间戳）被改写成
	// `1.2345678901234568e+15` —— 上游/下游 ID 对不上，且与 payload.go BUG-2
	// 刻意用 UseNumber 保住原文的纪律直接冲突（TrimRounds 在 client.go 里
	// **先于** prepareBodyOptFull 执行，主管线救不回来）。
	var messages []any
	dec := json.NewDecoder(bytes.NewReader(rawMsgs))
	dec.UseNumber()
	if derr := dec.Decode(&messages); derr != nil {
		return body
	}
	trimmed := trimMessages(messages, keep)
	if len(trimmed) == len(messages) {
		return body // 没裁就别重编码（保持逐字节一致）
	}
	// 审查 W4-U1-33（P2）：用 marshalNoEscape 而不是 json.Marshal —— 后者把
	// `<`/`>`/`&` 转成 \u003c 等，开启 keep_recent_rounds 后出站 body 与客户端原文
	// 成片不同，正是 payload.go 要避免的「指纹面」。
	//
	// 未改的剩余面（记录在案）：root 是 map[string]json.RawMessage，Marshal 会把
	// 顶层键按字典序重排，丢掉客户端的键序。要修得引入「顶层值字节区间」扫描器，
	// 改动面与收益不成比例（JSON 对象本就无序，上游解码不看键序），且本功能
	// 默认关闭（keep_recent_rounds=0）。这里只保证**值**逐字节不变。
	enc, err := marshalNoEscape(trimmed)
	if err != nil {
		return body
	}
	root["messages"] = enc
	out, err := marshalNoEscape(root)
	if err != nil {
		return body
	}
	return out
}
