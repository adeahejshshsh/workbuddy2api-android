package upstream

import (
	"net/http"
	"testing"
)

// BUG-3：裸数字 12153 只在会话失效的状态码上认，不能被任意 body 里的数字命中。
func TestClassify12153NeedsStatusGate(t *testing.T) {
	// 401 + 12153 → 会话失效
	if got := Classify(http.StatusUnauthorized, `{"code":12153,"msg":"offline"}`); got != ErrSessionDead {
		t.Fatalf("401+12153 判成 %v，期望 ErrSessionDead", got)
	}
	// 400 + 文本里出现 12153（token 数/requestId 片段）→ 不许判会话失效
	if got := Classify(http.StatusBadRequest, `{"msg":"prompt is too long: 12153 tokens"}`); got == ErrSessionDead {
		t.Fatal("400 里出现 12153 被误判成会话失效（会永久禁用账号）")
	}
	// 文本标记不设门槛
	if got := Classify(http.StatusBadRequest, `{"msg":"Offline user session not found"}`); got != ErrSessionDead {
		t.Fatalf("文本标记判成 %v，期望 ErrSessionDead", got)
	}
}
