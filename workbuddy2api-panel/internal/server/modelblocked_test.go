package server

// 全池模型配额锁的响应形态（上游 2026-10-26 `436631c`）。
//
// 配额锁（6004/6000：上游明说用量已超出频率限制、将在某时刻重置）在重置前重试
// **必然同样失败**，所以该回 429 + 上游业务码、且**不发 Retry-After** —— 客户端
// 普遍把这个头当成「睡多久」的指令（codebuddy CLI 直接 delayMs = retry-after×1000，
// 不设上限），20 分钟的冷却就成了 20 分钟静默挂起。

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// quotaLockBody 上游 6004「使用量已超出频率限制」的真实形态（带重置墙钟）。
const quotaLockBody = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-10-25 23:00:00 重置","requestId":"rq-quota"}`

// quotaLockBody6000 6000 形态：与 6004 同族不同码，靠「切换其他模型」提示区分。
const quotaLockBody6000 = `{"code":6000,"msg":"您的使用量已超出频率限制，您也可以切换其他模型继续使用","requestId":"rq-quota-6000"}`

func decodeErrCode(t *testing.T, body []byte) (int, string, string) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    int    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("bad response JSON: %v body=%s", err, body)
	}
	return resp.Error.Code, resp.Error.Type, resp.Error.Message
}

// 全池该模型都被 6004 用量配额锁住 → 429 + 6004 + 上游原文，不带 Retry-After。
func TestChatModelQuotaLockedReturns429(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "q1", AccessToken: "at-q1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "q2", AccessToken: "at-q2", ExpiresAt: 9999999999},
	)
	reset := time.Now().Add(20 * time.Minute)
	for _, uid := range []string{"q1", "q2"} {
		p.CooldownSoftForModel(uid, time.Minute, reset, "glm-5.2", quotaLockBody)
	}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("全池配额锁时不应打上游，auth=%q", authz)
		return 500, "", false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 429 {
		t.Fatalf("code=%d body=%s (want 429)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("配额锁不该发 Retry-After（客户端会照睡到解锁），got %q", got)
	}
	code, typ, msg := decodeErrCode(t, rec.Body.Bytes())
	if code != 6004 {
		t.Errorf("error.code=%d want 6004（数字业务码，客户端据此判不可重试）", code)
	}
	if typ != "insufficient_quota" {
		t.Errorf("error.type=%q want insufficient_quota", typ)
	}
	if !strings.Contains(msg, "您的使用量已超出频率限制") {
		t.Errorf("message 应透传上游原文，got %q", msg)
	}
	if strings.Contains(rec.Body.String(), "model_busy") {
		t.Errorf("配额锁不该报成 model_busy（那是可重试语义）：%s", rec.Body)
	}
}

// 6000 的锁必须报成 6000，不能一律标成 6004 —— 否则锁因与上游原文对不上，
// 面板上也无从区分是「用量频率限制」还是「使用量超出」。
func TestChatModelQuota6000ReportsOwnCode(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "q1", AccessToken: "at-q1", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("q1", time.Minute, time.Now().Add(10*time.Minute), "glm-5.2", quotaLockBody6000)
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("配额锁时不应打上游")
		return 500, "", false
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 429 {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	code, _, _ := decodeErrCode(t, rec.Body.Bytes())
	if code != 6000 {
		t.Errorf("error.code=%d want 6000（不能一律标成 6004）", code)
	}
}

// 混因（一个号配额锁、一个号 14003 瞬时忙）**不算**配额锁：14003 等一等就好，
// 报成不可重试会让客户端放弃一个马上能用的模型。
func TestChatModelQuotaMixedWithBusyKeepsRetryable(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "m1", AccessToken: "at-m1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "m2", AccessToken: "at-m2", ExpiresAt: 9999999999},
	)
	p.CooldownSoftForModel("m1", time.Minute, time.Now().Add(20*time.Minute), "glm-5.2", quotaLockBody)
	p.CooldownModelBusy("m2", "glm-5.2", "14003 model busy")
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("全池都被模型级冷却挡住时不应打上游")
		return 500, "", false
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 503 {
		t.Fatalf("混因应回可重试的 503，got %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_busy") {
		t.Errorf("混因应保留 model_busy 语义：%s", rec.Body)
	}
}

// 瞬时忙分支的 Retry-After 必须封顶：客户端把它当「睡多久」且不设上限，
// 把半小时的冷却原样报出去就是半小时静默挂起。
func TestChatModelBusyRetryAfterCapped(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "b1", AccessToken: "at-b1", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("b1", time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "14003 model busy")
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("不应打上游")
		return 500, "", false
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 503 {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	got := rec.Header().Get("Retry-After")
	if got == "" {
		t.Fatal("瞬时忙应带 Retry-After")
	}
	secs, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("Retry-After=%q 不是整数", got)
	}
	if secs > maxRetryAfterSec {
		t.Fatalf("Retry-After=%d 超过上限 %d（30 分钟冷却被原样报出）", secs, maxRetryAfterSec)
	}
}
