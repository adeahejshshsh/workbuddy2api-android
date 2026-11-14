package server

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const highClientBody = `{"model":"cn:deepseek-v4.1-flash","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`

// TestBudgetWasRaisedDetection 判定「网关抬过额度」：客户端原始额度低于本轮下限。
func TestBudgetWasRaisedDetection(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		floor int64
		want  bool
	}{
		{"high 客户端 2048 / 下限 8192", highClientBody, upstream.MinOutputBudget("high"), true},
		{"客户端 65536 / 下限 8192", `{"model":"m","max_tokens":65536}`, upstream.MinOutputBudget("high"), false},
		{"没给额度（目录补的，不算抬）", `{"model":"m"}`, upstream.MinOutputBudget("high"), false},
		{"没有下限（档位未知）", highClientBody, 0, false},
		{"别名 max_completion_tokens", `{"model":"m","max_completion_tokens":1024}`, upstream.MinOutputBudget("max"), true},
		// 出站抬额度对这两种请求不动（见 upstream.ensureMinOutputBudget），这里也不能当成「抬过」。
		{"探活级额度 max_tokens=1", `{"model":"m","max_tokens":1}`, upstream.MinOutputBudget("high"), false},
		{"客户端自己关了思考", `{"model":"m","max_tokens":2048,"thinking":{"type":"disabled"}}`, upstream.MinOutputBudget("high"), false},
		// 关不掉思考的模型：抬过额度也没法用「关思考续写」补回来，判定仍为 true（是否续写由
		// limitedButContinuable 再看 thinkingSwitchable）。
		{"模型不在可关思考名单里但客户端带了 thinking", `{"model":"glm-5.2","max_tokens":2048,"thinking":{"type":"enabled"}}`, upstream.MinOutputBudget("high"), true},
	}
	for _, c := range cases {
		r := &continueReader{body: []byte(c.body), budgetFloor: c.floor}
		r.limitSet, r.limit = bodyOutputLimit(r.body)
		if got := r.budgetWasRaised(); got != c.want {
			t.Errorf("%s: budgetWasRaised=%v want %v", c.name, got, c.want)
		}
	}
}

// TestContinueAllowedWhenBudgetRaised 网关抬过额度后，finish=length 必须续写。
//
// 线上矩阵实测（2026-11-08，effort=high、客户端 max_tokens=2048 → 抬到 8192）：
// 首段把 8192 用尽、正文 4942 token 处断句，finish=length。此时生效的上限是网关选的
// 8192，不是客户端的 2048——不续写等于「抬了额度还是断」，只是把断点从 2048 挪到 8192。
func TestContinueAllowedWhenBudgetRaised(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(highClientBody), "1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(testSeg1Text)), upstream.MinOutputBudget("high"))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（抬过额度后必须续接）", len(*reqs))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte((*reqs)[0]), &body); err != nil {
		t.Fatalf("续写请求体非法: %v", err)
	}
	// 续写段：上限不缩小（至少下限），且关思考避免再被额度卡住。
	if got, _ := body["max_tokens"].(float64); int64(got) < 8192 {
		t.Fatalf("续写段 max_tokens=%v want >= 8192", body["max_tokens"])
	}
	if th, _ := body["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Fatalf("抬过额度的续写段应关思考，实际 thinking=%v", body["thinking"])
	}
	if !strings.Contains(string(out), "继续") {
		t.Fatalf("客户端应看到续接的正文:\n%s", out)
	}
}

// TestContinueStillBlockedForPureClientLimit 客户端额度已经够大（网关没抬）时，
// finish=length 仍然是它的预期语义，不续写。
func TestContinueStillBlockedForPureClientLimit(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":8192,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(testSeg1Text)), upstream.MinOutputBudget("high"))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("续写请求数=%d want 0（客户端自己要的长度不续）", len(*reqs))
	}
	if !strings.Contains(string(out), `"finish_reason":"length"`) {
		t.Fatalf("length 终态应原样下发:\n%s", out)
	}
}

// TestContinueWhenClientLimitIsLongAnswer 客户端额度 ≥ clientLimitLongAnswer 时必须
// 续写：这个数是客户端随手给的上限，不是「只要这么短」。
//
// 实测（global:deepseek-v4.1-flash-sg，max_tokens=32000，要求「数到 20000」）：
// completion=32000 整、finish=length、正文 47853 字符停在「9792,」之后；同一提示词
// 不带额度能写 60660 token 自然收尾——即上游是「给多少切多少」，不是有 32000 硬上限。
func TestContinueWhenClientLimitIsLongAnswer(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, testSeg2Text })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":32000,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(testSeg1Text)), upstream.MinOutputBudget("high"))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("续写请求数=%d want 1（额度 ≥ %d 必须续接）", len(*reqs), clientLimitLongAnswer)
	}
	s := string(out)
	if !strings.Contains(s, "继续") || strings.Contains(s, `"finish_reason":"length"`) {
		t.Fatalf("客户端应看到续接后的完整收尾:\n%s", s)
	}
}
