package server

import "strings"

// sseframe.go SSE 帧的 `data:` 载荷提取——**唯一实现**。
//
// 规范允许 `data:` 后面**没有空格**（`data:{"..."}`），而三处消费点此前各写一套：
//   - upstream/sse.go 的解析（已显式兼容两种形态）；
//   - messages.go / responses.go 的适配器（只认带空格的 `data: `）。
//
// 当前不会错，因为后两者的输入是 upstream.StreamHint 的输出、它恒写 `"data: "`。但
// 一旦帧源变化（复用原始透传、或 StreamHint 增加直通分支），整条流会被静默丢光 ——
// 症状是「流式返回空内容且无错误」，最难查的一类。所以收敛到一处。
func cutDataPayload(frame string) (string, bool) {
	rest, ok := strings.CutPrefix(frame, "data:")
	if !ok {
		return "", false
	}
	// 可选单个前导空格（SSE 规范：`data:` 后若紧跟一个空格则去掉它）。
	return strings.TrimPrefix(rest, " "), true
}
