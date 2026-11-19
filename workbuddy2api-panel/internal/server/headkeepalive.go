package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// sseCommitWriter 记录流式响应是否已经提交（写过字节或 flush 过）。
//
// 首帧心跳本身在 upstream.StreamHint 里（见 upstream/headkeepalive.go）：只在本账号上游已回
// 200 之后、首帧到达之前补注释帧，且全部由 StreamHint 所在的 handler 协程写出。心跳提交了
// 200 之后，这一次尝试仍可能被首帧否决而换号；若随后全部失败，末端错误不能再回非 200 +
// JSON——往已提交的 SSE 流里塞 JSON，客户端收到的是既非帧也非 JSON 的脏数据。
// writeOpenAIError* 据 Committed() 把错误改写成协议内 error 帧 + [DONE]。
//
// W2-08：vetoFirstFrame 打开时，它同时是「首帧否决」的执行点。流式请求的第一个
// 完整 SSE 事件会先扣在 pending 里判一次：
//   - 第一个事件是 error 帧 → 不写、不提交，记下 payload 交给 handler 回真实 HTTP 状态码
//     （429/400/503）。此前这种情况下 200 已经提交，客户端只看到「200 + error 帧」，
//     Claude Code / Codex 的退避重试（按 HTTP 状态码判 retryable）全部失效；
//   - 第一个事件是非 error（正常 delta / 注释心跳）→ 原样写出并转为直写，延迟零变化。
//
// 只套在流式请求上（见 chatCompletions）。所有写都在 handler 协程里，不需要锁；
// 也不碰响应头（http.Header 是普通 map，并发 Set 会让运行时直接 fatal）。
type sseCommitWriter struct {
	http.ResponseWriter
	committed bool

	// 首帧否决状态（W2-08）。vetoFirstFrame=false 时这些字段全部不参与。
	vetoFirstFrame bool
	decided        bool   // 首个事件已判定：true 后不再缓冲
	vetoed         bool   // 首事件是 error 帧：后续写入一律丢弃，直到 DisarmVeto
	vetoPayload    string // 被否决的 error 帧 data 载荷（供 handler 分类/文案）
	pending        []byte // 首事件未成形前的缓冲（上限 headVetoMaxBytes）
}

// headVetoMaxBytes 首帧缓冲上限：超过就当成正常流直接提交。
// 单个正常 delta 远小于 64KB；真遇到巨大首帧时，宁可失去否决能力也不能无限缓存。
const headVetoMaxBytes = 64 << 10

func (c *sseCommitWriter) Write(p []byte) (int, error) {
	if !c.vetoFirstFrame || c.decided {
		if c.vetoed {
			// 已否决：StreamHint 后续补的 [DONE] 等字节全部丢弃，客户端只拿真实
			// HTTP 错误，不能再混入 SSE 片段。
			return len(p), nil
		}
		c.committed = true
		return c.ResponseWriter.Write(p)
	}
	// 非 SSE 形态的写（典型是 writeOpenAIError 的 JSON）必须直接透传：
	// 流式请求在 StreamHint 之前就可能失败（无可用账号/规则拦截/预算不足），
	// 那些错误是正经 HTTP JSON，不能被当成「还没成形的首事件」扣在缓冲里。
	if len(c.pending) == 0 && !isSSEWrite(p) {
		c.decided = true
		c.committed = true
		return c.ResponseWriter.Write(p)
	}
	c.pending = append(c.pending, p...)
	if ev, ok := firstSSEEvent(c.pending); ok {
		if payload, isErr := sseErrorPayload(ev); isErr {
			c.vetoed = true
			c.decided = true
			c.vetoPayload = payload
			c.pending = nil
			return len(p), nil
		}
		// 首个事件正常：整批（含后续已缓冲字节）一次性写出并转入直写。
		c.decided = true
		out := c.pending
		c.pending = nil
		c.committed = true
		_, err := c.ResponseWriter.Write(out)
		return len(p), err
	}
	if len(c.pending) > headVetoMaxBytes {
		c.decided = true
		out := c.pending
		c.pending = nil
		c.committed = true
		_, err := c.ResponseWriter.Write(out)
		return len(p), err
	}
	return len(p), nil
}

// Flush 透传 http.Flusher —— 不实现它 StreamHint 就拿不到 flusher，流式会被整段缓冲。
//
// 首事件判定前不 flush：flush 会把响应头提交掉，首帧否决窗口随之关闭。这不影响
// 换段/保活补帧的既有约束（continue.go：首段零数据帧时不补保活），因为那些注释帧
// 本身也是「首个完整事件」，会走 Write 的非 error 分支正常提交。
func (c *sseCommitWriter) Flush() {
	if c.vetoed {
		// 已否决：StreamHint 在写帧后仍会调 flush；不能让它把「零字节」标成已提交，
		// 否则 handler 会误判「状态码改不了」，又把真实 HTTP 错误降级成 SSE error 帧。
		return
	}
	if c.vetoFirstFrame && !c.decided {
		return
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		c.committed = true
		f.Flush()
	}
}

// Unwrap 供 http.ResponseController 追到底层 writer（Go 1.20+ 的 Flush 探测路径）。
func (c *sseCommitWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// Committed 报告响应头是否已提交（= 已经写过字节，不能再回非 200 + JSON）。
func (c *sseCommitWriter) Committed() bool { return c.committed }

// Vetoed 报告首帧是否被否决，并返回被扣下的 error 帧载荷。
func (c *sseCommitWriter) Vetoed() (string, bool) { return c.vetoPayload, c.vetoed }

// DisarmVeto 放弃否决：后续写入直接透传。
//
// 用于「已经确认零字节提交，可以回真实 HTTP 错误」的分支——写 JSON 之前必须调用，
// 否则 Write 会继续把字节当成被否决的 SSE 丢弃。
func (c *sseCommitWriter) DisarmVeto() {
	c.vetoFirstFrame = false
	c.vetoed = false
	c.decided = true
	c.committed = false // 被否决的帧一个字节都没写出去，状态码仍可改
	c.pending = nil
}

// Settle 在 StreamHint 返回后调用：首事件若始终没成形（只有半截元数据行/零帧），
// 丢掉半截缓冲。
//
// 注意**不**关闭否决窗口：静默空回合会换参数重发一次，那次尝试仍应享有首帧否决
// （否则重发段的首帧错误又只能 200 + SSE error）。若调用方随后直接写 JSON 错误，
// Write 的 isSSEWrite 分支会立即转直写，不会被扣住。
func (c *sseCommitWriter) Settle() {
	if c.vetoFirstFrame && !c.decided {
		c.pending = nil
	}
}

// isSSEWrite 判断一次写是否以 SSE 行前缀开头（data:/event:/id:/retry:/注释）。
// 只用于区分「SSE 帧」与「JSON 错误体」；Write 契约允许任意分块，但本仓的
// StreamHint 用 io.WriteString 整行写出，JSON 走 writeJSON 单次 Write，边界稳定。
func isSSEWrite(p []byte) bool {
	for _, prefix := range [...]string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(p, []byte(prefix)) {
			return true
		}
	}
	return false
}

// firstSSEEvent 从缓冲里切出第一个完整 SSE 事件（以空行结束）。
func firstSSEEvent(b []byte) ([]byte, bool) {
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 {
		return b[:i+2], true
	}
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		return b[:i+4], true
	}
	return nil, false
}

// sseErrorPayload 判断一个 SSE 事件的 data 载荷是否为上游 error 帧。
// 注释帧、只有 event:/id: 的元数据帧、[DONE]、正常 delta 都返回 false。
func sseErrorPayload(event []byte) (string, bool) {
	// SSE 允许多行 data: 拼成一条消息（上游美化输出时会这样发）。先按规范
	// 收集全部 data 行再判 JSON，否则首行只是 "{" 的半截形态会被误判成正常帧。
	var parts []string
	for _, raw := range strings.Split(string(event), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		payload, ok := cutDataPayload(line)
		if ok {
			parts = append(parts, payload)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	joined := strings.Join(parts, "\n")
	if joined == "[DONE]" {
		return "", false
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(joined), &probe) == nil &&
		len(probe.Error) > 0 && string(probe.Error) != "null" {
		return joined, true
	}
	return "", false
}

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
