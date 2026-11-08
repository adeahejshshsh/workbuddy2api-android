// output_budget.go 输出预算下限：客户端挡不住「高档位思考把自己那份额度吃光」的
// 组合错误，网关兜住它（上游 2b9b773）。
//
// 现象（上游 2026-11-08 生产实测，cn:deepseek-v4.1-flash，要求写 3000+ 字长文档）：
//
//	effort=high + max_tokens=2048 → total=4096 think=2048 正文=2048 finish=length
//	effort=high + 不给额度        → total=16720 think=2998 正文=13722 finish=stop
//
// 即：思考与正文在**同一份额度**里结算时，客户端给的 2048 先被思考吃掉一半，正文
// 只剩 1024 token 就断在句子中间 —— 用户看到的就是「回答写到一半停住」。客户端不知道
// 模型的思考要花多少，只能拍一个数；这个组合错误只能由网关修。
//
// 抬的是**上限**不是目标长度：模型写完自然停（实测 high+大额度收尾在 13722 token，
// 不会因为上限变大而多写一个字），所以不改变答案长度、不额外计费。
package upstream

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
)

// minBudgetByEffort 各思考档位下「正文还能写得出来」的最小输出预算。
// 取值依据见文件头：下限按该档思考的上界再留一倍给正文（4k/8k/16k）。
var minBudgetByEffort = map[string]int64{
	"minimal": 4096,
	"low":     4096,
	"medium":  6144,
	"high":    8192,
	"xhigh":   12288,
	"max":     16384,
}

// minOutputBudgetProbe 低于这个额度一律不动：官方客户端用 max_tokens=1 做 key 探活，
// 抬上去没有意义，还会让探活变慢。
const minOutputBudgetProbe = 16

// ensureMinOutputBudget 在「客户端显式给了 max_tokens」且「低于该档下限」时抬到下限。
//
// 不动的四种情形（都不猜）：
//   - 没有显式额度（客户端没给，交给上游默认）；
//   - 额度是探活级（< 16）；
//   - 关闭思考（thinking.type=disabled / effort=off/none/disabled）；
//   - 档位为空或未知。
func ensureMinOutputBudget(obj map[string]any) {
	effort := effortValue(obj)
	if effort == "" {
		return
	}
	if effort == "off" || effort == "none" || effort == "disabled" {
		return
	}
	if th, ok := obj["thinking"].(map[string]any); ok {
		if t, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(t), "disabled") {
			return
		}
	}
	floor, ok := minBudgetByEffort[effort]
	if !ok {
		return
	}
	cur, ok := maxTokensValue(obj)
	if !ok || cur <= 0 || cur < minOutputBudgetProbe {
		return
	}
	if cur >= floor {
		return
	}
	setMaxTokensValue(obj, floor)
	log.Printf("WARN: [upstream] output budget floor: max_tokens %d → %d (effort=%s；否则思考会吃掉正文额度，回答断在半句)", cur, floor, effort)
}

// MinOutputBudget 该思考档位下「正文还能写得出来」的最小输出预算；档位未知返回 0。
//
// 供续写/救援段复用（continue.go / starved.go）：那些段是从**客户端原始 body**
// 重建的（不含首段被抬过的 max_tokens），而它们往往关掉思考，prepareBody 的下限逻辑
// 会跳过（没有思考就不存在「思考吃光预算」）。若不显式带上，续写段会继续用客户端
// 那个装不下正文的小额度——线上实测正是这样：首段 8192 全被思考吃掉 → 救援段只拿到
// 1024 → 正文 1946 字就断。
func MinOutputBudget(effort string) int64 {
	return minBudgetByEffort[strings.ToLower(strings.TrimSpace(effort))]
}

// EnsureMinBudget 把 obj 的 max_tokens 抬到不低于 floor（floor<=0 或当前额度是
// 探活级时不动）。返回是否改动。
func EnsureMinBudget(obj map[string]any, floor int64) bool {
	if floor <= 0 {
		return false
	}
	v, ok := maxTokensValue(obj)
	if !ok || v <= 0 || v < minOutputBudgetProbe || v >= floor {
		return false
	}
	obj["max_tokens"] = json.Number(strconv.FormatInt(floor, 10))
	return true
}

// BudgetFloorFor 从**客户端原始请求体**推算本轮生效档位对应的输出预算下限
// （续写/救援段用；见 MinOutputBudget）。
//
// 为什么不直接把 prepareBody 里抬额的那个值回传：那条管线在 upstream 内部，返回值
// 是改写后的 bytes，档位已经埋在 JSON 里。这里按同一条顺序（策略 → injectThinking
// → 档位降级）在**副本**上重算一遍档位——纯函数、无副作用，且与出站时用的档位同源，
// 不会出现「首段按 high 抬额、续写段按客户端原发 low 算下限」的漂移。
func (c *Client) BudgetFloorFor(body []byte, realm string) int64 {
	if len(body) == 0 {
		return 0
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return 0
	}
	efforts, defs := c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm)
	if realmKey(realm) == "global" {
		efforts, defs = globalEffortMap(efforts, defs)
	} else {
		efforts, defs = cnEffortMap(efforts, defs)
	}
	modelName, _ := obj["model"].(string)
	applyEffortPolicy(obj, modelName, efforts[modelName], c.effortPolicy.Load(), realm)
	injectThinking(obj, lookupDefaultEffort(defs, modelName))
	normalizeReasoningEffort(obj, efforts)
	return MinOutputBudget(effortValue(obj))
}

// effortValue 取最终生效的思考档位（snake/camel 双字段，小写归一）。
func effortValue(obj map[string]any) string {
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if v, ok := obj[k].(string); ok {
			if s := strings.ToLower(strings.TrimSpace(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

// maxTokensValue 取客户端显式给的输出额度（数字或数字字符串）。
func maxTokensValue(obj map[string]any) (int64, bool) {
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		v, has := obj[k]
		if !has {
			continue
		}
		switch n := v.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i, true
			}
		case float64:
			return int64(n), true
		case string:
			var i int64
			if err := json.Unmarshal([]byte(n), &i); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

// setMaxTokensValue 写回同一个键（客户端用哪个键就改哪个，不擅自换字段名）。
func setMaxTokensValue(obj map[string]any, v int64) {
	lit := json.Number(strconv.FormatInt(v, 10))
	if _, has := obj["max_tokens"]; has {
		obj["max_tokens"] = lit
		return
	}
	obj["max_completion_tokens"] = lit
}
