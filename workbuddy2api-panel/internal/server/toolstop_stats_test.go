// toolstop_stats_test.go 钉住 issue #44 的观测链路：从 SSE 逐帧抓出
// 「本流是否出现过非空 tool_calls」与「最后一个非空 finish_reason」。
//
// 判据必须看**数组长度**而不是字段存在：上游每帧都带 "tool_calls":[]，
// 按字段存在判定会把零调用的静默回合误判成「有工具调用」，WARN 永远不触发。
package server

import (
	"strings"
	"testing"
	"time"
)

func TestStatsReaderSeesNoToolCallsOnStop(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"choices":[{"delta":{"content":"。","tool_calls":[]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"","tool_calls":[]},"finish_reason":"stop"}]}`,
	)
	if s.SawToolCalls() {
		t.Fatal("空数组不应算作见过 tool_calls")
	}
	if got := s.FinishReason(); got != "stop" {
		t.Fatalf("finish=%q want stop", got)
	}
}

func TestStatsReaderSeesToolCalls(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}]}`,
	)
	if !s.SawToolCalls() {
		t.Fatal("非空 tool_calls 应被记录")
	}
	if got := s.FinishReason(); got != "tool_calls" {
		t.Fatalf("finish=%q want tool_calls", got)
	}
}
