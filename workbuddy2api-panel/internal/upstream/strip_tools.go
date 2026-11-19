package upstream

import "encoding/json"

// StripToolDescriptions 摘掉 tools[].function.description，其余字节尽量不动。
//
// 用途（11128 的最后一层抢救）：上游对「非官方渠道」的判定里，除了 system 提示词的
// 整段指纹，还有一条更宽的连续原文匹配 —— 客户端自带工具的描述文案（DSH 系客户端
// 的 `dshmarketplace_*` 工具说明里直接写着品牌词与渠道词）。这类文本在
// `sanitizeTools` 里已经过一遍身份类规则，但**规则表永远滞后于客户端版本**：
// 新版本换一句措辞、加一段说明，就能让整条请求 400 "Illegal API invocation from an
// unapproved channel"（code 11128），而且是在降级到中性提示词之后仍然被拦 ——
// 因为触发点根本不在 messages 里。
//
// 摘描述而不是删工具：name / parameters 原样保留，模型仍能正常调用；描述只是
// 「什么时候用它」的提示，损失远小于整条请求失败。只在**已经失败一次**之后调用，
// 不会影响正常请求的提示质量。
//
// 返回 (新 body, 是否真的摘掉了东西)。没有 tools、或所有工具都没有 description 时
// 返回 (nil, false)，调用方据此避免无意义的重试。
func StripToolDescriptions(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return nil, false
	}
	// 顶层用 RawMessage 承接：只有 tools 会被重写，其它字段逐字节原样带回去
	// （model / messages / stream_options 这些一个都不能因为一次抢救被重新编码）。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false
	}
	rawTools, ok := top["tools"]
	if !ok || len(rawTools) == 0 {
		return nil, false
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return nil, false
	}
	stripped := false
	for _, t := range tools {
		rawFn, ok := t["function"]
		if !ok {
			continue
		}
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(rawFn, &fn); err != nil {
			continue
		}
		if _, has := fn["description"]; !has {
			continue
		}
		delete(fn, "description")
		nb, err := json.Marshal(fn)
		if err != nil {
			continue
		}
		t["function"] = nb
		stripped = true
	}
	if !stripped {
		return nil, false
	}
	nb, err := json.Marshal(tools)
	if err != nil {
		return nil, false
	}
	top["tools"] = nb
	out, err := json.Marshal(top)
	if err != nil {
		return nil, false
	}
	return out, true
}
