package server

// 请求归档上下文（logging.request_archive_context）：把会话上下文记进请求事件，
// 供面板「请求记录」点行展开看。
//
// 这一组用例钉住三件事：① 开关关时事件里不出现 context（缺省行为，敏感内容不该
// 默认落盘）；② 开时 system/messages/prompt 都进；③ 超长时按「丢最旧轮次 →
// 截断最后一条」收敛到 16KB 以内，且**始终是合法 JSON**（面板按结构渲染）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

func TestArchiveContextKeepsSystemMessagesPrompt(t *testing.T) {
	body := []byte(`{"system":"sys","messages":[{"role":"user","content":"hi"}],"prompt":"一只猫"}`)
	raw := archiveContext(body)
	if raw == nil {
		t.Fatal("应抽到上下文")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("不是合法 JSON: %v (%s)", err, raw)
	}
	if got["system"] != "sys" || got["prompt"] != "一只猫" {
		t.Fatalf("got=%v", got)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", got["messages"])
	}
}

// 非法 JSON / 空对象不产出上下文（nil 时事件里不出现该字段）。
func TestArchiveContextEmptyInputs(t *testing.T) {
	for _, body := range []string{`not json`, `{}`, `{"model":"glm-5.2"}`} {
		if raw := archiveContext([]byte(body)); raw != nil {
			t.Fatalf("%q 应返回 nil，got %s", body, raw)
		}
	}
}

// 超预算时丢最旧的轮次，且输出仍是合法 JSON、体积不超上限。
func TestArchiveContextTrimsOldestRounds(t *testing.T) {
	big := strings.Repeat("x", 4000)
	var msgs []string
	for i := 0; i < 20; i++ {
		msgs = append(msgs, `{"role":"user","content":"`+big+`"}`)
	}
	raw := archiveContext([]byte(`{"messages":[` + strings.Join(msgs, ",") + `]}`))
	if raw == nil {
		t.Fatal("应抽到上下文")
	}
	if len(raw) > contextMaxBytes {
		t.Fatalf("体积 %d 超上限 %d", len(raw), contextMaxBytes)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("截断后不是合法 JSON: %v", err)
	}
	kept, _ := got["messages"].([]any)
	if len(kept) == 0 || len(kept) >= 20 {
		t.Fatalf("应丢掉了最旧的若干轮，保留 %d 条", len(kept))
	}
}

// 单条就超预算：content 被截成串，仍是合法 JSON。
func TestArchiveContextTruncatesSingleHugeMessage(t *testing.T) {
	huge := strings.Repeat("y", 40<<10)
	raw := archiveContext([]byte(`{"messages":[{"role":"user","content":"` + huge + `"}]}`))
	if raw == nil {
		t.Fatal("应抽到上下文")
	}
	if len(raw) > contextMaxBytes {
		t.Fatalf("体积 %d 超上限 %d", len(raw), contextMaxBytes)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	s, _ := first["content"].(string)
	if !strings.Contains(s, "超长已截断") {
		t.Fatalf("content 未截断: %d 字节", len(s))
	}
}

// 只有超长 system、没有 messages 时也要收敛（大段 system 提示词曾能撑到几 MB）。
func TestArchiveContextTruncatesHugeSystem(t *testing.T) {
	huge := strings.Repeat("z", 40<<10)
	raw := archiveContext([]byte(`{"system":"` + huge + `"}`))
	if raw == nil {
		t.Fatal("应抽到上下文")
	}
	if len(raw) > contextMaxBytes {
		t.Fatalf("体积 %d 超上限 %d", len(raw), contextMaxBytes)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
}

// 开关：关（缺省）时事件里没有 context；开时有，且内容是这次请求的 messages。
func TestRequestLogContextSwitch(t *testing.T) {
	body := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"你好"}]}`

	run := func(recordCtx bool) *reqlog.Event {
		up := newFakeUpstream(t, func(string) (int, string, bool) {
			return 200, sseOK, true
		})
		reqLog := reqlog.New(reqlog.Config{})
		h := NewHandler(Config{
			Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
			Upstream:      up,
			RequestLog:    reqLog,
			RecordContext: recordCtx,
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		s := reqLog.Snapshot()
		if len(s.Recent) != 1 {
			t.Fatalf("recent=%+v", s.Recent)
		}
		return &s.Recent[0]
	}

	if e := run(false); e.Context != nil {
		t.Fatalf("开关关时不该记录上下文: %s", e.Context)
	}
	e := run(true)
	if e.Context == nil {
		t.Fatal("开关开时应记录上下文")
	}
	var got struct {
		Messages []any `json:"messages"`
	}
	if err := json.Unmarshal(e.Context, &got); err != nil {
		t.Fatalf("上下文不是合法 JSON: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages=%v", got.Messages)
	}
}

// /v1/messages 与 /v1/responses 都先转成 chat 体再进内核，所以同一处填充就覆盖
// 三条入站协议——这里钉住转协议后仍能拿到上下文。
func TestRequestLogContextAcrossProtocols(t *testing.T) {
	cases := []struct{ path, body string }{
		{"/v1/messages", `{"model":"glm-5.2","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"glm-5.2","stream":true,"input":"hi"}`},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, sseOK, true
			})
			reqLog := reqlog.New(reqlog.Config{})
			h := NewHandler(Config{
				Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream:      up,
				RequestLog:    reqLog,
				RecordContext: true,
			})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", c.path, strings.NewReader(c.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			s := reqLog.Snapshot()
			if len(s.Recent) != 1 || s.Recent[0].Context == nil {
				t.Fatalf("该协议未记录上下文: %+v", s.Recent)
			}
		})
	}
}

// 日预算必须真的累计：chatStat.done() 是唯一记账点，漏调的话 /status 里
// daily_budget.used 永远是 0、闸门永不触发（曾出现 add/observeCache 全无调用方）。
func TestChatStatDoneCommitsBudget(t *testing.T) {
	// 末帧带 usage.credit=0.5：done() 应把它记进日预算。
	const sseCredit = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"credit\":0.5}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseCredit, true
	})
	h := NewHandler(Config{
		Pool:        testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    up,
		DailyBudget: 100,
	})
	if used, _, _, _ := h.budget.snapshot(); used != 0 {
		t.Fatalf("初始应为 0，实际 %v", used)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if used, _, _, _ := h.budget.snapshot(); used <= 0 {
		t.Fatalf("done() 未把本轮实扣记进日预算，used=%v", used)
	}
}
