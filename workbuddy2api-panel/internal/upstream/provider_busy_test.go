package upstream

// 11134「供应商过载」按容量信号处置（上游 4830686）。
//
// 它不是账号问题：2026-11-07 生产实测 15 分钟内跨 6 个不同账号同时返回。落 ErrServer
// 会按 5xx 喂熔断（threshold 3 → 30m），高峰期把整片健康号打进冷却；换号重试撞的是
// 同一个 provider，还会打断账号级前缀缓存。所以归 ErrUpstreamBusy：不罚号、不轮转。

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyProviderUnavailable(t *testing.T) {
	body := `{"code":11134,"msg":"the model provider is temporarily unavailable, please retry later or switch to another model"}`
	if got := Classify(http.StatusInternalServerError, body); got != ErrUpstreamBusy {
		t.Fatalf("11134 判成 %v，期望 ErrUpstreamBusy", got)
	}
	// 只认结构化 code：没有业务码的 5xx 仍是 ErrServer（不猜文案）
	noCode := `{"msg":"the model provider is temporarily unavailable, please retry later"}`
	if got := Classify(http.StatusInternalServerError, noCode); got != ErrServer {
		t.Fatalf("无业务码的 5xx 判成 %v，期望 ErrServer", got)
	}
	// 429 仍按限流（code 11134 只在 5xx 形态下出现；这里确认不误伤普通 429）
	if got := Classify(http.StatusTooManyRequests, `{"msg":"too many requests"}`); got != ErrSoftRate {
		t.Fatalf("普通 429 判成 %v", got)
	}
}

func TestParseQueueRetryAfterRetryPolicy(t *testing.T) {
	// 11134 的等待口径在 retry_policy.retry_after（没有 Retry-After 头）
	d, ok := parseQueueRetryAfter([]byte(`{"code":11134,"retry_policy":{"retry":true,"retry_after":8}}`))
	if !ok || d != 8*time.Second {
		t.Fatalf("retry_policy.retry_after 解析成 %v ok=%v，期望 8s", d, ok)
	}
	// 6020 的 data.retry_after 仍然生效（优先级更高）
	d, ok = parseQueueRetryAfter([]byte(`{"code":6020,"data":{"retry_after":40},"retry_policy":{"retry_after":9}}`))
	if !ok || d != 40*time.Second {
		t.Fatalf("data.retry_after 应优先：%v ok=%v", d, ok)
	}
	// 都没有 → 交给调用方兜底
	if _, ok := parseQueueRetryAfter([]byte(`{"code":11134}`)); ok {
		t.Fatal("没有等待口径时不该返回 ok")
	}
}
