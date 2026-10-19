package upstream

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游「200 + error 帧」是真实形态（6004 限流 / 内容审核 / 11158 等级拒绝）。
// Aggregate 必须把它升级成 ErrorFrame 交回调用方，不能当普通事件吃掉 ——
// 吃掉就会合成 content="" + finish_reason:"stop" 的假 200，调用方还会
// NoteSuccess 把正在限流的号洗成健康号。
func TestAggregateErrorFrameIsNotFakeSuccess(t *testing.T) {
	body := "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"error\":{\"code\":6004,\"message\":\"模型请求过于频繁，将在 2026-10-19 21:00:00 重置\"}}\n\n" +
		"data: [DONE]\n\n"
	_, err := Aggregate(strings.NewReader(body))
	if err == nil {
		t.Fatal("error frame must not aggregate into a success response")
	}
	if !IsErrorFrame(err) {
		t.Fatalf("want *ErrorFrame, got %T: %v", err, err)
	}
	if IsEmptyStreamError(err) {
		t.Fatal("error frame is not an empty stream: it carries an upstream reason")
	}
	var ef *ErrorFrame
	if !errors.As(err, &ef) || !strings.Contains(ef.Payload, "6004") {
		t.Fatalf("payload lost: %+v", ef)
	}
	// 分类必须与流式路径同一口径：6004 是软限流。
	if k := FrameKind(ef.Payload); k != ErrSoftRate {
		t.Fatalf("FrameKind=%v want ErrSoftRate", k)
	}
}

// 正常流仍然成功（回归保护：别把普通帧误判成 error）。
func TestAggregateNormalStreamStillOK(t *testing.T) {
	body := "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":3}}\n\ndata: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	ch := resp["choices"].([]any)
	msg := ch[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hi" {
		t.Fatalf("content=%v", msg["content"])
	}
}

// SSE 规范允许 "data:{...}"（冒号后无空格）。只认 "data: " 会让这种上游变成
// 零有效事件 → 误报 502 upstream_parse。
func TestAggregateAcceptsDataWithoutSpace(t *testing.T) {
	body := "data:{\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata:[DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(body))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	ch := resp["choices"].([]any)
	msg := ch[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "ok" {
		t.Fatalf("content=%v", msg["content"])
	}
}

// 上游中途断流：必须补一帧 error + [DONE]，并返回可识别的「读上游失败」，
// 而不是裸 return（客户端拿到悬挂流、handler 还把它当客户端断连洗白账号）。
func TestStreamHintUpstreamReadErrorClosesStream(t *testing.T) {
	// 第一帧正常，之后读错误。
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		pw.CloseWithError(errors.New("connection reset by peer"))
	}()
	rec := httptest.NewRecorder()
	err := StreamHint(rec, pr, nil)
	if err == nil {
		t.Fatal("want error on upstream read failure")
	}
	if !IsUpstreamReadError(err) {
		t.Fatalf("want errUpstreamRead, got %v", err)
	}
	if IsClientWriteError(err) {
		t.Fatal("upstream read error must not look like a client write error")
	}
	out := rec.Body.String()
	if !strings.Contains(out, "upstream_read_error") {
		t.Fatalf("missing error frame: %s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "data: [DONE]") {
		t.Fatalf("stream must end with exactly one [DONE]: %q", out)
	}
}

// 写客户端失败必须可识别（handler 据此记 Interrupted 并保持账号健康）。
func TestStreamHintClientWriteErrorIsTyped(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"))
		pw.Close()
	}()
	err := StreamHint(failWriter{}, pr, nil)
	if err == nil || !IsClientWriteError(err) {
		t.Fatalf("want client write error, got %v", err)
	}
}

// failWriter 模拟客户端断连：任何写都失败。
type failWriter struct{}

func (failWriter) Header() http.Header         { return http.Header{} }
func (failWriter) Write(p []byte) (int, error) { return 0, errors.New("broken pipe") }
func (failWriter) WriteHeader(statusCode int)  {}
