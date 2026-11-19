package server

// W2-03 回归：MCP 代理回执必须有界捕获。
//
// 旧实现 httptest.NewRecorder 全量缓冲 → MarshalIndent → structuredContent
// 再序列化：b64 图片单请求峰值约 200MB。现在由 mcpCaptureWriter 限制在
// mcpProxyMaxBytes，超过即停止捕获并让工具回 isError。

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestMCPCaptureWriterIsBounded(t *testing.T) {
	w := newMCPCaptureWriter()
	big := bytes.Repeat([]byte("x"), mcpProxyMaxBytes+4096)
	n, err := w.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("Write 应报告全部消费（避免 handler 误判客户端断开）：n=%d err=%v", n, err)
	}
	if !w.overflow {
		t.Fatal("超过上限必须置 overflow")
	}
	if w.buf.Len() > mcpProxyMaxBytes {
		t.Fatalf("捕获缓冲 %d 字节，超过上限 %d", w.buf.Len(), mcpProxyMaxBytes)
	}
	// 再来一次也不该继续长：上限是硬的。
	_, _ = w.Write(big)
	if w.buf.Len() > mcpProxyMaxBytes {
		t.Fatalf("第二次写后缓冲 %d，超过上限 %d", w.buf.Len(), mcpProxyMaxBytes)
	}
}

func TestMCPCaptureWriterKeepsSmallResponse(t *testing.T) {
	w := newMCPCaptureWriter()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if w.overflow || w.buf.String() != `{"ok":true}` {
		t.Fatalf("小回执应原样保留：overflow=%v body=%q", w.overflow, w.buf.String())
	}
	if w.status != http.StatusOK {
		t.Fatalf("status=%d want 200", w.status)
	}
}

// 默认不美化：content.text 必须是紧凑 JSON（不再为每个回执生成一份缩进副本）。
func TestMCPProxyCallDoesNotPrettyPrintByDefault(t *testing.T) {
	t.Setenv("WB2A_MCP_PRETTY", "")
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"generate_image",`+
		`"arguments":{"prompt":"一只猫"}}}`)
	result, _ := out["result"].(map[string]any)
	content, _ := result["content"].([]any)
	txt, _ := content[0].(map[string]any)["text"].(string)
	if strings.Contains(txt, "\n  ") {
		t.Fatalf("默认不应 MarshalIndent：%q", txt)
	}
	if !strings.Contains(txt, "cos.example") {
		t.Fatalf("text 应含完整回执：%q", txt)
	}
}
