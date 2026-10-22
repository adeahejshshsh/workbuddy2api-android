// starved.go 「思考吃光预算」的救援重发（misakano7545 `b0320cf1`）。
//
// 现象（真机实测，cn:deepseek-v4.1-flash，2026-10-22）：上游把 reasoning 与正文字节
// 算在**同一个** max_tokens 预算里，思考没有独立上限字段。客户端给一个偏小的预算时，
// 思考可以独吞整份额度 —— 结束于 finish_reason=length、正文零字节、没有 tool_calls，
// 客户端看到的就是「空回复」：
//
//	max_tokens=60/100/150/200/300 → thinking 吃满，正文 0 字节
//	max_tokens=400                → 思考 166~272，正文开始出现（时好时坏）
//	thinking:{type:disabled} + 同预算 → 正文正常、finish=stop
//
// 救援动作是「**同预算、关思考**重发一次」：不放大客户端要的额度（超发是对客户端的
// 语义违约，本身也是个可判别形态），只把预算从思考手里拿回来。只在「这一轮什么都
// 没产出」时触发 —— 正常回复、有正文的正常截断都不碰。
//
// 官方客户端为什么不会撞上：它的出站 max_tokens 来自模型配置的 maxOutputTokens
// （未配置则整条省略、走上游默认 32000），从不发一个比思考还小的额度。
package server

import (
	"encoding/json"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// minStarvedBudget 低于此额度不救援：官方客户端用 max_tokens=1 做 key 探活，
// 那种请求按原样失败即可，不值得为它多发一次上游请求。
const minStarvedBudget = 16

// starvedRetryBody 把请求体改写成救援形态：thinking 关掉、删掉思考档位，其余
// （max_tokens / tools / messages）原样保留。
//
// 返回 false 的情形都是「不猜」：没有 thinking 开关的模型、已经关着思考的、
// 额度小到不值得重发的、body 不可解析。
func starvedRetryBody(body []byte) ([]byte, bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	if bt := outputBudget(obj); bt > 0 && bt < minStarvedBudget {
		return nil, false
	}
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		// 客户端通常不自己带 thinking（档位/开关由上游管线注入），这里按**模型能力**
		// 判断能不能关：拿不到开关的模型不猜别的手段。
		model, _ := obj["model"].(string)
		if !upstream.ThinkingSwitchSupported(model) {
			return nil, false
		}
		th = map[string]any{}
	}
	if t, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(t), "disabled") {
		return nil, false
	}
	th["type"] = "disabled"
	obj["thinking"] = th
	// 关思考就别再带档位：上游对 disabled + effort 的组合语义未取证，
	// 而思考管线的 disabled 分支本来就要求两者不共存（见 upstream/thinking.go）。
	delete(obj, "reasoning_effort")
	delete(obj, "reasoningEffort")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// aggregateStarved 聚合体是否属于「预算被思考吃光、什么也没产出」。
// 三个条件同时成立才算：finish_reason=length、正文为空、没有 tool_calls。
// 有正文的截断是客户端额度语义的正常结果，不在此列。
func aggregateStarved(resp map[string]any) bool {
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		return false
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return false
	}
	if fr, _ := c["finish_reason"].(string); fr != "length" {
		return false
	}
	m, _ := c["message"].(map[string]any)
	if m == nil {
		return false
	}
	if s, _ := m["content"].(string); strings.TrimSpace(s) != "" {
		return false
	}
	if tcs, _ := m["tool_calls"].([]any); len(tcs) > 0 {
		return false
	}
	return true
}

// outputBudget 读请求体里的输出限额（三个别名），未携带 → 0。
func outputBudget(obj map[string]any) int {
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		switch v := obj[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return 0
}

// bodySetsOutputLimit 客户端是否显式给了输出限额（救援只在这种请求上做：
// 没给限额时空回复另有「额度未知」的成因，不该我们替它决定）。
func bodySetsOutputLimit(body []byte) bool {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return false
	}
	return outputBudget(obj) > 0
}
