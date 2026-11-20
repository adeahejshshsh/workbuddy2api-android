package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestOnOutboundMatchesWire 11128 快照必须拿到**真正发出去**的字节。
//
// 线上教训：旧实现落的是 handler 手里的入站 body，于是「快照里有指纹」与
// 「上游仍然 400」可以同时成立（实际出站早被合并/脱敏改过）。这里钉住两件事：
//   - 回调拿到的字节与对端收到的字节逐字节相同（小 body 不压缩）；
//   - 回调看到的是 prepareBody 之后形态 —— 相邻 system 已合并成一条。
func TestOnOutboundMatchesWire(t *testing.T) {
	var wire []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{HTTP: &http.Client{}, ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}

	var hook []byte
	rc, _, _, err := c.ChatStreamContext(context.Background(), a, []byte(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"a"},
			{"role":"system","content":"b"},
			{"role":"user","content":"hi"}
		]}`), "", ChatMeta{OnOutbound: func(b []byte) {
		hook = append([]byte(nil), b...)
	}})
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	if len(hook) == 0 {
		t.Fatal("OnOutbound 未被调用")
	}
	if !bytes.Equal(hook, wire) {
		t.Fatalf("回调字节与对端收到的不同：hook=%d wire=%d", len(hook), len(wire))
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(hook, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "a\n\nb" {
		t.Fatalf("回调里应是**已合并**的形态，实际 %+v", got.Messages)
	}
}
