package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 文案用的 base 必须与**实际配置**一致（报告 #7）：写死官方地址时，配了
// cn.chat_base / global.chat_base 自建上游的部署，报错文案里给的是错域名。
func TestUpstreamBaseForRealm(t *testing.T) {
	// 未配置上游 → 回落官方默认（cn/global 各一条；空 realm 按 cn）。
	h := &Handler{}
	if got := h.upstreamBaseForRealm("cn"); got != "https://copilot.tencent.com" {
		t.Fatalf("cn base = %q", got)
	}
	if got := h.upstreamBaseForRealm("global"); got != "https://www.workbuddy.ai" {
		t.Fatalf("global base = %q", got)
	}
	if got := h.upstreamBaseForRealm(""); got != "https://copilot.tencent.com" {
		t.Fatalf("空 realm base = %q", got)
	}

	// 配了自定义 base → 必须用它。
	h2 := &Handler{cfg: Config{Upstream: &upstream.Client{
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
	}}}
	if got := h2.upstreamBaseForRealm("cn"); got != "https://cn.example" {
		t.Fatalf("自定义 cn base = %q", got)
	}
	if got := h2.upstreamBaseForRealm("global"); got != "https://global.example" {
		t.Fatalf("自定义 global base = %q", got)
	}
}

// TestTransportBrief 传输层错误要折叠空白并截断，别把整条 URL 塞进客户端界面。
func TestTransportBrief(t *testing.T) {
	if transportBrief(nil) != "" {
		t.Fatal("nil 应返回空串")
	}
	got := transportBrief(errors.New("dial tcp 43.1.2.3:443:\n\tconnect: connection refused"))
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("未折叠空白: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("丢了关键信息: %q", got)
	}
	long := transportBrief(errors.New(strings.Repeat("x", 500)))
	if len([]rune(long)) > 210 || !strings.HasSuffix(long, "…") {
		t.Fatalf("未截断: %d runes", len([]rune(long)))
	}
}
