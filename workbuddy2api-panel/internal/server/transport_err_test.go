package server

import (
	"errors"
	"strings"
	"testing"
)

// TestUpstreamBaseForRealm 文案用的 base 必须与路由常量一致（cn/global 各一条）。
func TestUpstreamBaseForRealm(t *testing.T) {
	if got := upstreamBaseForRealm("cn"); got != "https://copilot.tencent.com" {
		t.Fatalf("cn base = %q", got)
	}
	if got := upstreamBaseForRealm("global"); got != "https://www.workbuddy.ai" {
		t.Fatalf("global base = %q", got)
	}
	// 未知/空 realm 按 cn 处理（与 resolveModel 的裸名=cn 口径一致）。
	if got := upstreamBaseForRealm(""); got != "https://copilot.tencent.com" {
		t.Fatalf("空 realm base = %q", got)
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
