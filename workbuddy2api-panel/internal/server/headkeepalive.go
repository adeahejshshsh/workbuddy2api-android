package server

import (
	"encoding/json"
	"net/http"
)

// sseCommitWriter 记录流式响应是否已经提交（写过字节或 flush 过）。
//
// 首帧心跳本身在 upstream.StreamHint 里（见 upstream/headkeepalive.go）：只在本账号上游已回
// 200 之后、首帧到达之前补注释帧，且全部由 StreamHint 所在的 handler 协程写出。心跳提交了
// 200 之后，这一次尝试仍可能被首帧否决而换号；若随后全部失败，末端错误不能再回非 200 +
// JSON——往已提交的 SSE 流里塞 JSON，客户端收到的是既非帧也非 JSON 的脏数据。
// writeOpenAIError* 据 Committed() 把错误改写成协议内 error 帧 + [DONE]。
//
// 只套在开了首帧心跳的流式请求上（见 chatCompletions）。所有写都在 handler 协程里，不需要锁；
// 也不碰响应头（http.Header 是普通 map，并发 Set 会让运行时直接 fatal）。
type sseCommitWriter struct {
	http.ResponseWriter
	committed bool
}

func (c *sseCommitWriter) Write(p []byte) (int, error) {
	c.committed = true
	return c.ResponseWriter.Write(p)
}

// Flush 透传 http.Flusher —— 不实现它 StreamHint 就拿不到 flusher，流式会被整段缓冲。
func (c *sseCommitWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		c.committed = true
		f.Flush()
	}
}

// Unwrap 供 http.ResponseController 追到底层 writer（Go 1.20+ 的 Flush 探测路径）。
func (c *sseCommitWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Committed 报告响应头是否已提交（= 已经写过字节，不能再回非 200 + JSON）。
func (c *sseCommitWriter) Committed() bool { return c.committed }

// writeErrorFrame 在响应头已提交（注释帧已写出）时把失败收敛成协议内错误：
// 一条 error 帧 + 一条 [DONE]，与 StreamHint「已开流只能 200」的兜底同形态。
func (c *sseCommitWriter) writeErrorFrame(status int, typ, code, msg, hint string) {
	c.writeErrorFrameExtra(status, typ, code, msg, hint, nil)
}

// writeErrorFrameExtra 同 writeErrorFrame，另在 error 对象里附加 extra 字段。
// 响应头已提交后不能再改 HTTP 状态码，而有些客户端（ZCode）只按状态码/业务码
// 判「能不能重试」——把 statusCode=429 这类信号塞进协议内错误体，客户端才能
// 按可重试处理，而不是把整轮对话判死。
func (c *sseCommitWriter) writeErrorFrameExtra(status int, typ, code, msg, hint string, extra map[string]any) {
	e := map[string]any{"message": msg, "type": typ, "code": code}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	for key, v := range extra {
		if _, exists := e[key]; !exists {
			e[key] = v
		}
	}
	body, err := json.Marshal(map[string]any{"error": e})
	if err != nil {
		return
	}
	_, _ = c.Write([]byte("data: " + string(body) + "\n\ndata: [DONE]\n\n"))
	c.Flush()
}
