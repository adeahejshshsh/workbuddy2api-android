// errbrief.go 把上游 JSON 错误体压成「给人看的一句话」。
//
// 背景（用户可见问题）：上游错误体本身就是一份 JSON 文档，例如
//
//	{"code":11158,"msg":"model [gpt-6-astra] is not allowed for user type [personal_free_user]",
//	 "requestId":"3c1e...","displayMsg":{"en":"...","zh":"当前账号类型暂不支持该模型。"},
//	 "actions":["SUBMIT_FEEDBACK","COPY_ERROR","SWITCH_MODEL"]}
//
// 网关此前按「上游原文透传」把它整段塞进 error.message —— 于是任何客户端看到的都是
// 满屏反斜杠转义：`"message":"{\"code\":11158,\"msg\":\"...\"}"`。原文的价值在于
// code / msg / requestId，不在那层 JSON 语法本身。
//
// 本文件把这份 JSON 拆成：一句人话（Brief）+ 结构化字段（Code/RequestID/Display/Msg），
// 客户端既能直接显示，也能拿到可编程的字段；非 JSON 体退化为折叠空白 + 截断。
package upstream

import (
	"encoding/json"
	"strings"
)

// briefMaxRunes 人话文案上限（按 rune 截断，避免把动辄几 KB 的 body 拖进上下文）。
const briefMaxRunes = 400

// rawMaxRunes 非 JSON 体（HTML 错误页等）折叠后的截断上限。
const rawMaxRunes = 240

// ErrorBrief 上游错误的可读摘要。
type ErrorBrief struct {
	Brief     string `json:"brief"`                // 给人看的一句话（永远非空）
	Code      string `json:"code,omitempty"`       // 上游业务 code，如 "11158"
	RequestID string `json:"request_id,omitempty"`
	Display   string `json:"display,omitempty"`    // 上游本地化文案（优先中文）
	Msg       string `json:"msg,omitempty"`        // 上游英文 msg
}

// BriefError 解析上游错误体。空体 → 返回空 Brief（调用方自行兜底文案）。
func BriefError(body string) ErrorBrief {
	s := strings.TrimSpace(body)
	if s == "" {
		return ErrorBrief{}
	}
	var br ErrorBrief
	if obj := decodeErrObject(s); obj != nil {
		br.Code = anyToCodeString(obj["code"])
		br.Msg = anyToString(obj["msg"])
		if br.Msg == "" {
			br.Msg = anyToString(obj["message"])
		}
		br.RequestID = anyToString(obj["requestId"])
		if br.RequestID == "" {
			br.RequestID = anyToString(obj["request_id"])
		}
		br.Display = pickDisplay(obj["displayMsg"])
	}
	br.Brief = composeBrief(br, s)
	return br
}

// IsModelNotAllowedForUserType 报告上游错误是否「该模型不允许当前账号类型」(11158)。
//
// 与 11102（该后端无此模型）的区别：11102 是模型不存在，11158 是**账号等级不够**
// ——换号可能有用（另一个号等级更高），换模型也一定有用。两者都该把该号对该模型
// 短期避让，但给客户端的文案必须说清是"等级不支持"，否则用户会一直重试。
//
// 判定口径与 IsModelBlocked 一致：只在结构化 code 字段上匹配，不拿整段文本匹配
// （避免把 "11158" 撞在 requestId / 其它数字上误判）。
func IsModelNotAllowedForUserType(body string) bool {
	obj := decodeErrObject(strings.TrimSpace(body))
	if obj == nil {
		return false
	}
	return anyToCodeString(obj["code"]) == "11158"
}

// decodeErrObject 解析错误体为对象；同时容忍 {"error":{...}} 的嵌套形态。
func decodeErrObject(s string) map[string]any {
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return nil
	}
	if inner, ok := obj["error"].(map[string]any); ok && inner != nil {
		// 嵌套形态：外层字段合并进来，内层优先（上游两套 wire 都在线上出现过）。
		merged := map[string]any{}
		for k, v := range obj {
			merged[k] = v
		}
		for k, v := range inner {
			merged[k] = v
		}
		return merged
	}
	return obj
}

// anyToString 宽容取字符串（数字也转，便于 msg 写成数字的畸形体）。
func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return trimFloat(t)
	case json.Number:
		return t.String()
	}
	return ""
}

// anyToCodeString 取业务 code：JSON 数字按整数渲染（11158 而非 11158.0）。
func anyToCodeString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return trimFloat(t)
	case json.Number:
		return t.String()
	}
	return ""
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		b, _ := json.Marshal(int64(f))
		return string(b)
	}
	b, _ := json.Marshal(f)
	return string(b)
}

// pickDisplay 取本地化文案：zh → zh-hant → 其它语言里第一个非空的。
func pickDisplay(v any) string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	for _, k := range []string{"zh", "zh-hans", "zh-cn", "zh-hant", "zh-tw", "en"} {
		if s := anyToString(m[k]); s != "" {
			return s
		}
	}
	// 没有常见键：取字典序第一个非空，保证确定性。
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if s := anyToString(m[k]); s != "" {
			return s
		}
	}
	return ""
}

// composeBrief 拼一句人话：优先"本地化文案 + 英文原文"，两者都有就并列；
// 都没有（非 JSON 体）→ 折叠空白后截断原文。
func composeBrief(br ErrorBrief, raw string) string {
	var parts []string
	if br.Display != "" {
		parts = append(parts, br.Display)
	}
	if br.Msg != "" && br.Msg != br.Display && !strings.Contains(br.Display, br.Msg) {
		parts = append(parts, br.Msg)
	}
	if len(parts) == 0 {
		return truncateRunes(collapseSpace(raw), rawMaxRunes)
	}
	out := strings.Join(parts, " ｜ ")
	if br.Code != "" {
		out += "（上游 code=" + br.Code + "）"
	}
	return truncateRunes(out, briefMaxRunes)
}

// collapseSpace 把连续空白（含换行/制表）压成单个空格：HTML 错误页 / 多行栈
// 塞进 message 时不再撑爆客户端界面。
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncateRunes 按 rune 截断并加省略号（避免截半个 UTF-8）。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…(已截断)"
}

// sortStrings 极简插入排序：只为 pickDisplay 的确定性，避免为一个键排序引入 import 依赖。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
