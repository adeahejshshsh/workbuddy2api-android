package server

// 积分闸（dailyBudget）的两处曾经失效：
//   ① 只在 chatCompletions 开头判 admit，出图构造了带 budget 的 chatStat 会**记账
//      但不 admit** —— 预算打穿后 chat 全 429、出图照跑、积分继续烧；
//   ② 累计值只在进程内存，重启即清零 —— 崩溃循环下唯一的成本止损线等于不存在。

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// creditSSE 末帧带 usage.credit，供 done() 记账进日预算。
const creditSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"credit\":0.5}}\n\n" +
	"data: [DONE]\n\n"

func budgetTestHandler(t *testing.T, limit float64, stateFile string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, creditSSE, true })
	return NewHandler(Config{
		Pool:            testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:        up,
		DailyBudget:     limit,
		BudgetStateFile: stateFile,
	})
}

// 出图也必须被积分闸拦住（此前它记账但不 admit）。
func TestBudgetGateCoversImages(t *testing.T) {
	h := budgetTestHandler(t, 0.1, "")
	// 先打穿预算：一次 chat 扣 0.5 > 0.1。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("首个请求应成功：code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 出图：预算已打穿 → 429，且**不该**打到上游。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"a cat"}`)))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("出图未被积分闸拦住：code=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

// Anthropic 入口要按 Anthropic 口径回 429（那类 SDK 解析不了 OpenAI 形状）。
func TestBudgetGateAnthropicShape(t *testing.T) {
	h := budgetTestHandler(t, 0.1, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("前置请求应成功：code=%d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), `"type":"error"`) {
		t.Fatalf("Anthropic 入口应按 Anthropic 错误形状回：%s", rec2.Body.String())
	}
}

// 累计值必须落盘：重启（新建 dailyBudget 读同一路径）后不归零。
func TestBudgetPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	h := budgetTestHandler(t, 100, path)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	used1, _, _, _ := h.budget.snapshot()
	if used1 <= 0 {
		t.Fatalf("应记到用量，实际 %v", used1)
	}

	// 模拟重启：同一路径重新构造。
	h2 := budgetTestHandler(t, 100, path)
	used2, _, _, _ := h2.budget.snapshot()
	if used2 != used1 {
		t.Fatalf("重启后累计值=%v，应为 %v（未落盘或未读回）", used2, used1)
	}
}

// 跨日：磁盘上是昨天的量，今天读回应为 0。
func TestBudgetDiscardsStaleDay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := newDailyBudget(100, path)
	b.day = "2000-01-01"
	b.used = 42
	b.saveLocked(time.Now(), true)

	b2 := newDailyBudget(100, path)
	if used, _, _, _ := b2.snapshot(); used != 0 {
		t.Fatalf("跨日应丢弃旧累计，实际 %v", used)
	}
}
