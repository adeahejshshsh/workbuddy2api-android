package server

// 静默回合救援（issue #44）：客户端声明了 tools，模型却只回一句 finish_reason=stop。
//
// 线上（2026-10-24）：DSH 带 82 个 tools 请求 deepseek-v4.1-flash-sg，上游回
//   finish_reason=stop with zero tool_calls
// 回合就此静默结束，用户只能手动再催一句 —— 日志里那条 WARN 是唯一痕迹。
//
// 救援手段：把 tool_choice 改成 required 重发一次。模型「有工具可用但一个都不调」
// 在 agent 回合里几乎总是卡住，强制 required 是标准恢复姿势；配合
// upstream.WithHoldUntilContent，重发时客户端一个字节都还没收到，完全无感。

import (
	"bytes"
	"encoding/json"
	"strings"
)

// forceToolChoiceRequired 把请求改成「必须调工具」（tool_choice=required）。
//
// 只在客户端**没有钉死**工具选择时才改：
//   - tool_choice 缺省 / "auto" / {"type":"auto"} → 改成 "required"
//   - "none" / {"type":"none"} / 具体函数名 / {"type":"function",...} → 一律不动
//
// 后者是明确意图（「这轮不要工具」或「只准调这个」），覆盖它会把「不要工具」变成
// 「必须调工具」，比静默回合更糟。
//
// 第二个返回值为 false 表示改不动 —— 调用方不该为它白跑一次上游。
func forceToolChoiceRequired(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return nil, false
	}
	// UseNumber 保住数字原文：救援重发不该顺手改写整份 body 的数字（默认解码把
	// 整数落成 float64，超过 2^53 的 seed/微秒时间戳会变成近似值）。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if dec.Decode(&obj) != nil {
		return nil, false
	}
	// 没有工具声明就无从「必须调」。
	_, hasTools := obj["tools"]
	_, hasFuncs := obj["functions"]
	if !hasTools && !hasFuncs {
		return nil, false
	}
	if tc, ok := obj["tool_choice"]; ok {
		switch v := tc.(type) {
		case string:
			if !strings.EqualFold(strings.TrimSpace(v), "auto") {
				return nil, false
			}
		case map[string]any:
			if typ, _ := v["type"].(string); !strings.EqualFold(strings.TrimSpace(typ), "auto") {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	obj["tool_choice"] = "required"
	// 不转义 HTML（报告 #11）：Marshal 默认把 < > & 转成 \u003c 等，与客户端原文不一致。
	out, err := marshalNoEscape(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// toolChoiceLabel 把客户端原发 tool_choice 压成一行短标签，供日志判断「是不是客户端
// 自己把工具关了」。空串 = 客户端没发该字段（默认 auto）。
func toolChoiceLabel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj map[string]any
	if dec.Decode(&obj) != nil {
		return ""
	}
	tc, ok := obj["tool_choice"]
	if !ok {
		return ""
	}
	switch v := tc.(type) {
	case string:
		return v
	case map[string]any:
		typ, _ := v["type"].(string)
		if strings.EqualFold(typ, "function") {
			if fn, ok := v["function"].(map[string]any); ok {
				if n, _ := fn["name"].(string); n != "" {
					return "function:" + n
				}
			}
		}
		return typ
	default:
		return "?"
	}
}
