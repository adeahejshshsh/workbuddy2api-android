package upstream

// X-Agent-Type 的场景枚举（上游 11-04 批次）：空 = 用户主对话 main；
// AgentTypeAuxiliary = 网关自发的辅助调用（工具探测、摘要、面板任务）。
//
// 此前我们恒发 main —— 网关自己的探测调用会自称「用户主对话」，与官方客户端
// 的行为不一致。这里钉住两条：默认仍发 main，显式 auxiliary 时按值下发。

import (
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestChatHeadersAgentType(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	t.Run("默认 main", func(t *testing.T) {
		req := httptest.NewRequest("POST", "https://example/v2/chat/completions", nil)
		c.ChatHeaders(req, a, "", ChatMeta{})
		if got := req.Header.Get("X-Agent-Type"); got != "main" {
			t.Fatalf("X-Agent-Type=%q want main", got)
		}
	})

	t.Run("auxiliary 按值下发", func(t *testing.T) {
		req := httptest.NewRequest("POST", "https://example/v2/chat/completions", nil)
		c.ChatHeaders(req, a, "", ChatMeta{AgentType: AgentTypeAuxiliary})
		if got := req.Header.Get("X-Agent-Type"); got != "auxiliary" {
			t.Fatalf("X-Agent-Type=%q want auxiliary", got)
		}
		// 归因头与主对话一致，不因场景改档（真机 auxiliary 分支同样带 craft）。
		if got := req.Header.Get("X-Agent-Intent"); got != "craft" {
			t.Fatalf("X-Agent-Intent=%q want craft", got)
		}
	})
}
