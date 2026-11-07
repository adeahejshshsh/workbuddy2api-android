package server

// 入站请求体上限（bodylimit.go）：五条读体入口都必须在上限内读完，超了回 413。
//
// 这组用例钉的是「上限在**读体之前**」这件事本身：四条转协议入口（messages /
// count_tokens / responses / images）此前各自 `io.ReadAll` 完才进 chatCompletions，
// 那层 MaxBytesReader 到那时已经晚了 —— 内存早就被吃掉了。只断言「400/413 有响应」
// 是测不出来的，必须让 body 真的超过上限并确认状态码是 413。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// oversizeJSON 造一个超过 limitMB 的合法 JSON 字符串值。
func oversizeJSON(limitMB int) string {
	pad := strings.Repeat("x", (limitMB<<20)+4096)
	return `{"model":"glm-5.2","messages":[{"role":"user","content":"` + pad + `"}]}`
}

func TestBodyLimitRejectsOversizeOnAllEntrypoints(t *testing.T) {
	const mb = 1
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		MaxBodyMB: mb,
	})
	body := oversizeJSON(mb)
	cases := []struct {
		path string
		body string
	}{
		{"/v1/chat/completions", body},
		{"/v1/messages", body},
		{"/messages/count_tokens", body},
		{"/v1/responses", body},
		{"/v1/images/generations", body},
		{"/v1/images/edits", body},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", c.path, strings.NewReader(c.body)))
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("code=%d want 413（上限未在读体前生效）body=%.120s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "max_body_mb") {
				t.Fatalf("413 文案应指向配置项：%s", rec.Body.String())
			}
		})
	}
}

// 上限之内的请求不该被误伤（否则「修 OOM」变成「所有请求都 413」）。
func TestBodyLimitAllowsNormalRequests(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:  up,
		MaxBodyMB: 1,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("正常请求被误判超限：%s", rec.Body.String())
	}
}

// max_body_mb<=0 时不再真的「不限制」：回落到 32MB 硬上限（只挡内存炸弹）。
func TestLimitBodyFallbackWhenUnlimited(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), MaxBodyMB: 0})
	rec := httptest.NewRecorder()
	got := h.limitBody(rec, httptest.NewRequest("POST", "/x", strings.NewReader("")))
	if want := int64(bodyLimitFallbackMB) << 20; got != want {
		t.Fatalf("fallback limit=%d want %d", got, want)
	}

	h2 := NewHandler(Config{Pool: testPoolWith(), MaxBodyMB: 7})
	got2 := h2.limitBody(httptest.NewRecorder(), httptest.NewRequest("POST", "/x", strings.NewReader("")))
	if want := int64(7) << 20; got2 != want {
		t.Fatalf("configured limit=%d want %d", got2, want)
	}
}

// countingReader 记录被读走的字节数，用来判断「超限后有没有排空」。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestBodyLimitDrainsBefore413 超限时先把剩余上传读掉再回 413。
//
// 为什么要这一步：直接 return 会让 net/http 关连接，客户端还在上传，只看到
// connection reset 而看不到那份 413（hub #799 的同款坑）。判据是「读掉的字节数」：
// 上限 1MB、body 3MB → 读体 1MB+1、排空再读 1MB，合计约 2MB；
// 既不能只读 1MB（没排空），也不能把 3MB 全读进来（那就失去了上限的意义）。
func TestBodyLimitDrainsBefore413(t *testing.T) {
	const mb = 1
	limit := int64(mb) << 20
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 3<<20) + `"}]}`

	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		MaxBodyMB: mb,
	})
	cr := &countingReader{r: strings.NewReader(body)}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", cr))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%.120s", rec.Code, rec.Body.String())
	}
	if cr.n < 2*limit {
		t.Fatalf("413 之前应排空剩余上传（至少读掉 2×上限 = %d 字节），实际只读了 %d", 2*limit, cr.n)
	}
	if cr.n > 2*limit+(64<<10) {
		t.Fatalf("不该把整个 3MB body 读进来（上限的意义就在这里），实际读了 %d 字节", cr.n)
	}
}
