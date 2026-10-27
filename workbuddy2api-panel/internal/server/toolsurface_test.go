package server

// issue #44 观测面：声明了 tools 的回合要把「声明数 / 实际调用数」写进归档事件。
//
// 这个缺陷最难的地方是**发生率不可测** —— 报告里的人靠客户端插件逐帧落盘才统计出
// 25%，网关侧翻日志根本算不出来。所以这两个字段本身就是修复的一部分。
// 反例（没声明 tools 的纯聊天）不得计入，否则分母是错的、比率没有意义。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// declaredToolsBody 复用 handler_test.go 里已有的定义（同一个「声明了一个工具」夹具）。

// sseToolCall 上游正常返回一次工具调用（delta.tool_calls + finish_reason=tool_calls）。
const sseToolCall = `data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]}}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
	"data: [DONE]\n\n"

func TestRequestMetricsToolsSurface(t *testing.T) {
	cases := []struct {
		name                     string
		reqBody                  string
		upstreamBody             string
		isStream                 bool
		wantDeclared, wantCalled int
	}{
		{
			name:         "流式：声明了 tools 但零调用（stop + 有正文）",
			reqBody:      `{"model":"glm-5.2","stream":true,"messages":[],` + declaredToolsBody + `}`,
			upstreamBody: sseOK,
			isStream:     true,
			wantDeclared: 1, wantCalled: 0, // 零工具收尾：issue #44 的分子
		},
		{
			name:         "流式：声明了 tools 且确实调用了",
			reqBody:      `{"model":"glm-5.2","stream":true,"messages":[],` + declaredToolsBody + `}`,
			upstreamBody: sseToolCall,
			isStream:     true,
			wantDeclared: 1, wantCalled: 1,
		},
		{
			name:         "非流式：声明了 tools 且确实调用了",
			reqBody:      `{"model":"glm-5.2","stream":false,"messages":[],` + declaredToolsBody + `}`,
			upstreamBody: sseToolCall,
			isStream:     false,
			wantDeclared: 1, wantCalled: 1,
		},
		{
			name:         "纯聊天：没声明 tools，不进统计",
			reqBody:      `{"model":"glm-5.2","stream":true,"messages":[]}`,
			upstreamBody: sseOK,
			isStream:     true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, c.upstreamBody, c.isStream
			})
			reqLog := reqlog.New(reqlog.Config{})
			h := NewHandler(Config{
				Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream:   up,
				RequestLog: reqLog,
			})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(c.reqBody)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			s := reqLog.Snapshot()
			if len(s.Recent) != 1 {
				t.Fatalf("want 1 event, got %d", len(s.Recent))
			}
			e := s.Recent[0]
			if e.ToolsDeclared != c.wantDeclared || e.ToolCalls != c.wantCalled {
				t.Fatalf("tools_declared=%d tool_calls=%d, want %d/%d",
					e.ToolsDeclared, e.ToolCalls, c.wantDeclared, c.wantCalled)
			}
		})
	}
}
