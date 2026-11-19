// sse_meta_test.go W1-03 / W1-04 回归。
//
// W1-03：帧内元数据行（event:/id:/retry:）必须有行数与字节双上限 —— 上游只要连发
// 这两类行而不发 data，缓冲就无上限增长（单行 8MB，万行 80GB，OOM 是进程级）。
// W1-04：多行 data 拼出的非 JSON 内容必须逐行带 `data: ` 前缀 —— 否则客户端按行
// 切分后第二行起没有字段名，按 SSE 规范直接忽略，内容静默只剩第一行。
package upstream

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamMetaLinesCapped 超过 sseMetaMaxLines 必须先把已缓冲的行刷成一个事件帧。
func TestStreamMetaLinesCapped(t *testing.T) {
	const extra = 3
	var b strings.Builder
	for i := 1; i <= sseMetaMaxLines+extra; i++ {
		fmt.Fprintf(&b, "event: e%d\n", i)
	}
	// 收尾给一个真实 data 帧：纯元数据 + [DONE] 会被判空流（既有契约），
	// 与本用例要验的「上限」无关。
	b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n")
	b.WriteString("data: [DONE]\n\n")

	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(b.String()), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()

	// 1) 一行都不能丢（超限只拆帧，不吞字节）。
	for i := 1; i <= sseMetaMaxLines+extra; i++ {
		if !strings.Contains(body, fmt.Sprintf("event: e%d\n", i)) {
			t.Fatalf("第 %d 行元数据丢失：\n%s", i, body)
		}
	}
	// 2) 上限处必须切开：第 sseMetaMaxLines 行后跟空行 = 前面的行已单独成帧。
	if !strings.Contains(body, fmt.Sprintf("event: e%d\n\n", sseMetaMaxLines)) {
		t.Fatalf("达到行数上限后未刷帧（缓冲仍无界）：\n%s", body)
	}
	// 3) 未到上限时不能切开（否则正常事件会被拆成多帧，event 类型与 data 分家）。
	if strings.Contains(body, fmt.Sprintf("event: e%d\n\n", sseMetaMaxLines-1)) {
		t.Fatalf("未到上限就刷帧，正常事件被拆散：\n%s", body)
	}
}

// TestStreamMetaOversizeLineFlushedAlone 单行就超过字节预算时单独成帧，不与后面的 data 粘连。
func TestStreamMetaOversizeLineFlushedAlone(t *testing.T) {
	big := "event: " + strings.Repeat("x", sseMetaMaxBytes)
	raw := big + "\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, big+"\n\n") {
		t.Fatalf("超预算的单行元数据应与后续 data 分帧（各自独立事件）：\n%s", truncateTest(body, 200))
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatal("后续 data 帧丢失")
	}
}

// TestStreamMultiLineNonJSONKeepsEveryLine W1-04：多行纯文本 data 必须逐行带前缀。
func TestStreamMultiLineNonJSONKeepsEveryLine(t *testing.T) {
	raw := "data: line-one\n" +
		"data: line-two\n" +
		"\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "data: line-one\ndata: line-two\n\n") {
		t.Fatalf("多行 data 未逐行加前缀（客户端按行切会丢第二行起）：\n%q", body)
	}
	// 不允许出现「没有 data: 前缀的裸内容行」。
	if strings.Contains(body, "\nline-two\n") {
		t.Fatalf("第二行裸写，客户端会按规范忽略：\n%q", body)
	}
	if !strings.Contains(body, "data: [DONE]\n\n") {
		t.Fatalf("[DONE] 应保持单行特例：\n%q", body)
	}
}

// TestWriteDataEventSingleLineByteIdentical 单行 payload 的输出必须与旧实现逐字节一致。
func TestWriteDataEventSingleLineByteIdentical(t *testing.T) {
	var b strings.Builder
	if err := writeDataEvent(&b, `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	if b.String() != "data: {\"a\":1}\n\n" {
		t.Fatalf("单行输出变了：%q", b.String())
	}
}

// truncateTest 截断日志用（避免失败时把整段 8KB 行全打出来）。
func truncateTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
