package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 模型降级路由 model_fallback 的行为测试。
//
// 需求（用户原话）：「v4.1 限流的时候自动路由到 v4 继续用，因为本质上这两个模型
// 是一样的，只是价格不一样」「v4.1 更便宜，优先用 v4.1」。所以降级方向是
// **便宜 → 贵**（可用性优先），且限流解除后要自动回 v4.1。

const rateLimit6004Body = `{"error":{"code":6004,"message":"该模型使用量超限，将在 10-22 09:40 重置"}}`

// okChatSSE 成功响应。用 SSE 而不是 json：网关默认按流式处理（请求体不带
// stream 字段时 peek.Stream 为 false，但 handler 仍走流式透传分支），给 json body
// 会被判为 upstream_parse 错误。
const okChatSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\u4f60\u597d\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// modelAwareUpstream 记录上游**实际收到的 model 序列**。
//
// 为什么不用 newFakeUpstream：它的 behavior 只拿得到 Authorization 头，
// 看不到请求 body 里的 model —— 而「上游收到的模型真的变了」正是降级的核心
// 验收点，断言不到就等于没测。
type modelAwareUpstream struct {
	mu     sync.Mutex
	models []string
	reply  []struct {
		status int
		body   string
	}
}

func (m *modelAwareUpstream) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var obj map[string]any
			name := ""
			if json.Unmarshal(raw, &obj) == nil {
				name, _ = obj["model"].(string)
			}
			m.mu.Lock()
			m.models = append(m.models, name)
			idx := len(m.models) - 1
			if idx >= len(m.reply) {
				idx = len(m.reply) - 1
			}
			rep := m.reply[idx]
			m.mu.Unlock()
			// 按 body 形态给 Content-Type：SSE 透传分支需要 text/event-stream，否则
			// 流解析器把它当 JSON 解，报 upstream_parse。
			ct := "application/json"
			if strings.HasPrefix(rep.body, "data: ") {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: rep.status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(rep.body)),
			}, nil
		})},
	}
}

func (m *modelAwareUpstream) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.models))
	copy(out, m.models)
	return out
}

func newFallbackHandler(fb map[string]string, up *upstream.Client) *Handler {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999})
	return NewHandler(Config{
		Pool:          p,
		Upstream:      up,
		MaxRotate:     3,
		ModelFallback: func() map[string]string { return fb },
	})
}

func doFallbackChat(h *Handler, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// TestModelFallbackOn6004 核心行为：v4.1 被 6004 → 同账号改用 v4 → 成功。
// 断言上游**依次**收到 [v4.1, v4]：证明真的换模型了，不是重试原模型。
func TestModelFallbackOn6004(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{429, rateLimit6004Body})
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})

	h := newFallbackHandler(map[string]string{"deepseek-v4.1-flash": "deepseek-v4-flash"}, up.client())
	rec := doFallbackChat(h, "deepseek-v4.1-flash")

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200（降级后应成功），body=%s", rec.Code, rec.Body)
	}
	got := up.seen()
	want := []string{"deepseek-v4.1-flash", "deepseek-v4-flash"}
	if len(got) != len(want) {
		t.Fatalf("上游收到 %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次上游模型 = %q, want %q（全序列 %v）", i, got[i], want[i], got)
		}
	}
}

// TestModelFallbackNoConfigKeepsOldBehavior 未配置降级表时零回归。
func TestModelFallbackNoConfigKeepsOldBehavior(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{429, rateLimit6004Body})

	h := newFallbackHandler(nil, up.client())
	doFallbackChat(h, "deepseek-v4.1-flash")

	for _, m := range up.seen() {
		if m != "deepseek-v4.1-flash" {
			t.Fatalf("未配置降级表却换了模型：上游收到 %v", up.seen())
		}
	}
}

// TestModelFallbackNotOn11102 11102（该后端无此模型）不得降级 ——
// 那是结构性缺失，换个等价模型同样可能没有，白烧一次调用。
func TestModelFallbackNotOn11102(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{400, `{"error":{"code":11102,"message":"model not found"}}`})

	h := newFallbackHandler(map[string]string{"deepseek-v4.1-flash": "deepseek-v4-flash"}, up.client())
	doFallbackChat(h, "deepseek-v4.1-flash")

	for _, m := range up.seen() {
		if m == "deepseek-v4-flash" {
			t.Fatalf("11102 不应降级，上游却收到等价模型：%v", up.seen())
		}
	}
}

// TestModelFallbackNoLoop 等价模型也 6004 时不得 A→B→A 死循环。
func TestModelFallbackNoLoop(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{429, rateLimit6004Body}) // 永远 6004

	h := newFallbackHandler(map[string]string{"deepseek-v4.1-flash": "deepseek-v4-flash"}, up.client())
	doFallbackChat(h, "deepseek-v4.1-flash")

	n := 0
	for _, m := range up.seen() {
		if m == "deepseek-v4-flash" {
			n++
		}
	}
	if n > 1 {
		t.Fatalf("等价模型被尝试 %d 次，应 ≤1（防 A→B→A 循环），全序列 %v", n, up.seen())
	}
}

// TestModelFallbackLeavesNoPersistentState ⭐ 用户明确要求的那条：
// 「限流解除后就自动回 v4.1」。
//
// 判据改为**直接检验性质**而不是模拟时间流逝：降级能否自动回退，本质取决于
// 「降级有没有留下持久状态」。留了状态（比如把 v4 也写进 modelOff / 把请求的
// 模型名持久改写），冷却到期也回不去；没留，curModel 只在本次请求内有效，
// 下次请求自然从 bareModel 重新开始。
//
// 为什么不在测试里拨冷却时间：6004 的 Until 受 pool.mu 保护，跨包改不了；
// BlockModelClear 又刻意只清 11102/11158（6004 有上游权威墙钟）。
// 与其为测试开一个生产代码不该有的后门，不如直接断言「降级不写持久状态」。
func TestModelFallbackLeavesNoPersistentState(t *testing.T) {
	up := &modelAwareUpstream{}
	for _, r := range []struct {
		status int
		body   string
	}{
		{429, rateLimit6004Body}, // v4.1 限流
		{200, okChatSSE},         // 降级到 v4 成功
	} {
		up.reply = append(up.reply, r)
	}
	h := newFallbackHandler(map[string]string{"deepseek-v4.1-flash": "deepseek-v4-flash"}, up.client())

	doFallbackChat(h, "deepseek-v4.1-flash")
	if got := up.seen(); len(got) < 2 || got[1] != "deepseek-v4-flash" {
		t.Fatalf("首次请求应降级，实际上游收到 %v", got)
	}

	// ① 等价模型不得被写进「手动关闭」表（那是永久语义，写了就永远回不去）。
	if got := h.cfg.Pool.ModelOffOf("u1"); len(got) != 0 {
		t.Errorf("降级不应写入手动关闭表（永久语义），实际 %+v", got)
	}

	// ② 等价模型不得被记成本模型的冷却 —— 否则下次请求 v4 会被无谓挡住。
	// ModelBlockedForRealm 查不到具体原因，改用「降级目标是否被判为冷却」间接验证：
	// 若 v4 被冷却，下面用 v4 直接发请求会拿到 503（model_busy）。
	up2 := &modelAwareUpstream{}
	up2.reply = append(up2.reply, struct {
		status int
		body   string
	}{200, okChatSSE})
	h.cfg.Upstream = up2.client()
	rec := doFallbackChat(h, "deepseek-v4-flash")
	if rec.Code != 200 {
		t.Fatalf("等价模型不应被降级动作冷却，直接请求它却得到 %d: %s", rec.Code, rec.Body)
	}
}

// TestModelFallbackWhenNoAccountForModel 全池该模型都在 6004 冷却时，
// 选号直接返回 nil —— 若只在「上游返回 6004」时降级，这条路径会先报 503，
// 恰好在最需要降级（全池被限流）的场景失效。本测试钉住该缺口。
func TestModelFallbackWhenNoAccountForModel(t *testing.T) {
	up := &modelAwareUpstream{}
	up.reply = append(up.reply, struct {
		status int
		body   string
	}{200, okChatSSE})

	h := newFallbackHandler(map[string]string{"deepseek-v4.1-flash": "deepseek-v4-flash"}, up.client())
	// 预先把 v4.1 标记为全池模型级冷却（模拟"上一次请求已撞过 6004"的稳态）。
	h.cfg.Pool.BlockModelBackoff("u1", "deepseek-v4.1-flash", "6004 test")
	h.cfg.Pool.CooldownModelBusy("u1", "deepseek-v4.1-flash", "6004 test")

	rec := doFallbackChat(h, "deepseek-v4.1-flash")
	if rec.Code != 200 {
		t.Fatalf("全池该模型冷却时应降级成功，实际 status=%d body=%s", rec.Code, rec.Body)
	}
	if got := up.seen(); len(got) != 1 || got[0] != "deepseek-v4-flash" {
		t.Fatalf("应直接改用等价模型，实际上游收到 %v", got)
	}
}
