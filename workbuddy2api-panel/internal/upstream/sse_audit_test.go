package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// newSSEStream 用一个固定的上游流跑 StreamHint，返回客户端收到的字节。
func newSSEStream(t *testing.T, upstream string, opts ...StreamOption) (string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(upstream), nil, opts...)
	return rec.Body.String(), err
}

// D2：`event:` / `id:` 这类帧内元数据行必须与随后的 data 行**同帧**。
// 此前它们各自被封成「行 + 空行」的独立事件，客户端收到的类型全部丢失。
func TestSSEMetaLinesStayInSameEvent(t *testing.T) {
	up := "event: message_start\nid: 42\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	got, err := newSSEStream(t, up)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	// 元数据行与 data 行之间不能出现空行（否则就是两个事件）。
	if strings.Contains(got, "event: message_start\n\n") {
		t.Fatalf("event 行被单独封成一个事件：\n%s", got)
	}
	if !strings.Contains(got, "event: message_start") || !strings.Contains(got, "data: ") {
		t.Fatalf("元数据行或 data 行丢了：\n%s", got)
	}
	// 同一帧内 event 必须在 data 之前。
	iEvent := strings.Index(got, "event: message_start")
	iData := strings.Index(got, "data: {")
	if iEvent < 0 || iData < 0 || iEvent > iData {
		t.Fatalf("event 行应在 data 行之前且同帧：\n%s", got)
	}
	// 事件边界：event 行到 data 行之间只有换行，没有空行。
	seg := got[iEvent:iData]
	if strings.Contains(seg, "\n\n") {
		t.Fatalf("event 与 data 之间出现了空行（被拆成两帧）：%q", seg)
	}
}

// D7：上游给了 finish_reason 但**不发** data: [DONE] 时，不能再补一帧 length。
// 此前 finishSeen 只在 repairFrame 里置位，正常路径不置 —— 于是正常终态之后又补一个
// finish_reason=length，客户端按最后一个终态判成「被截断」。
func TestSSENoExtraTruncationAfterFinish(t *testing.T) {
	up := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n"
	got, err := newSSEStream(t, up)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if strings.Contains(got, TruncationFinishReason) {
		t.Fatalf("正常终态之后又补了 %q：\n%s", TruncationFinishReason, got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("上游的 finish_reason 丢了：\n%s", got)
	}
}

// D6：心跳已经发过字节之后上游才断流，不能再走「换号重发」——
// 头已提交，换号重发会把 502 JSON 塞进已提交的 200 SSE。
func TestSSEHeartbeatThenBreakIsNotBeforeFlush(t *testing.T) {
	// 用一个会「先给一帧注释、再报错」的 body：注释帧写出后 committed 置位。
	body := &heartbeatThenError{beat: ": keepalive\n\n", err: io.ErrUnexpectedEOF}
	rec := httptest.NewRecorder()
	err := StreamHint(rec, body, nil)
	out := rec.Body
	if err != nil && strings.Contains(err.Error(), ErrStreamBrokeBeforeFlush.Error()) {
		t.Fatalf("心跳之后不该判成 before-flush（会导致往已提交的 200 里写 502）：%v\n输出：%s", err, out.String())
	}
}

type heartbeatThenError struct {
	beat string
	err  error
	done bool
}

func (h *heartbeatThenError) Read(p []byte) (int, error) {
	if !h.done {
		h.done = true
		return copy(p, h.beat), nil
	}
	return 0, h.err
}

func (h *heartbeatThenError) Close() error { return nil }
