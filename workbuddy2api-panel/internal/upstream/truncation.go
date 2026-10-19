// truncation.go 工具调用的残缺参数检测（吸收参考仓库 sse.ts:158-167
// isTruncatedArguments 语义）。
//
// 背景：SSE 流被截断（连接中断 / finish_reason==length）时，工具调用的 arguments
// 会只剩半截 JSON。此时网关若把脏参数原样交给客户端，客户端解析会报非法 JSON 并卡死会话。
// 参考仓库的处置是丢弃残缺调用并触发重试（报告 max-tokens），而非补成 {} 伪造合法外观。
//
// 关键区分：只把「非空但无法解析」视为截断。空串是合法的无参数工具；能解析但类型不对
// （标量 / 数组）属于模型输出错误，交给客户端 schema 校验回传即可，不在此判定。
package upstream

import (
	"encoding/json"
	"strings"
)

// isTruncatedArguments 判定工具参数字符串是否因分片丢失而残缺（区别于「该工具本就无参数」）。
//   - 空串 / 纯空白 → false（合法无参工具）；
//   - 非空但 JSON 解析失败 → true（截断）；
//   - 能解析（含 null/标量/数组等任何合法 JSON）→ false。
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}

// dropTruncatedToolCalls 过滤出 arguments 完整的 tool_call（返回新 slice 与被丢弃的数量）。
// 只依据 isTruncatedArguments 判定，不改动任何保留的调用（正例零改动）。
//
// 返回丢弃数是为了可观测（issue B14）：不少兼容上游"发完就 EOF"、根本不发
// data: [DONE]，此时被丢掉的调用会静默消失 —— 客户端看到"模型只说了话、工具没调"，
// 服务端日志里查不到任何痕迹。调用方据此打一条 WARN。
func dropTruncatedToolCalls(calls []map[string]any) (kept []map[string]any, dropped int) {
	kept = make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			kept = append(kept, call)
			continue
		}
		args, _ := fn["arguments"].(string)
		if isTruncatedArguments(args) {
			dropped++
			continue
		}
		kept = append(kept, call)
	}
	return kept, dropped
}
