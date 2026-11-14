package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// threeDFakeClient 带临时凭证的假上游（没有凭证的用例直接用 videoFake.client()）。
func threeDFakeClient(f *videoFake, tokens map[string]string) *upstream.Client {
	c := f.client()
	c.ThreeDCredential = upstream.NewStaticThreeDCredentials(tokens)
	return c
}

// 3D 请求固定的上游路径（探测与轮询都走它）。
const threeDProxyPathForTest = "/agenttool/v1/tcproxy"

// threeDHandler 造一个「3D 已开启 + 已配临时凭证」的 handler。
func threeDHandler(f *videoFake, token string) *Handler {
	return NewHandler(Config{
		Pool:          imagePool(),
		Upstream:      threeDFakeClient(f, map[string]string{"*": token}),
		ThreeDEnabled: true,
	})
}

func threeDSubmit(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.threeDGenerations(rec, httptest.NewRequest("POST", "/v1/3d/generations", strings.NewReader(body)))
	return rec
}

func threeDQuery(t *testing.T, h *Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.threeDTasks(rec, httptest.NewRequest("POST", "/v1/3d/tasks", strings.NewReader(`{"job_id":"`+id+`"}`)))
	return rec
}

// threeDSubmitReply / threeDTaskReply 包一层官方 tcproxy 的 {"Response":…} 信封。
func threeDSubmitReply(job string) string {
	return `{"Response":{"JobId":"` + job + `","Status":"PROCESSING","RequestId":"rid"}}`
}

func threeDTaskReply(status string, credit float64) string {
	s := `{"Response":{"Status":"` + status + `"`
	if credit > 0 {
		s += fmt.Sprintf(`,"ResultCreditConsumed":%g,"ResultFile3Ds":[{"Type":"GLB","Url":"model.glb"}]`, credit)
	}
	return s + `}}`
}

func TestThreeDUpstreamBody(t *testing.T) {
	body, err := threeDUpstreamBody(map[string]any{"prompt": "a chair", "face_count": 300000})
	if err != nil || body["Model"] != "3.1" || body["Prompt"] != "a chair" || body["FaceCount"] != 300000 {
		t.Fatalf("body=%v err=%v", body, err)
	}
	for _, req := range []map[string]any{
		{}, {"prompt": "x", "generate_type": "LowPoly"},
		{"prompt": "x", "generate_type": "Sketch"},
	} {
		if _, err := threeDUpstreamBody(req); err == nil {
			t.Fatalf("expected rejection: %v", req)
		}
	}
}

// 开关关闭（默认）：端点 403、MCP 工具面里没有 3D 工具、instructions 也不提 3D。
func TestThreeDDisabledByDefault(t *testing.T) {
	f := &videoFake{}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	if rec := threeDSubmit(t, h, `{"prompt":"a chair"}`); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), "three_d_disabled") {
		t.Fatalf("关闭时提交应 403 three_d_disabled，got %d %s", rec.Code, rec.Body)
	}
	if rec := threeDQuery(t, h, "job-1"); rec.Code != http.StatusForbidden {
		t.Fatalf("关闭时查询应 403，got %d %s", rec.Code, rec.Body)
	}
	if n := f.count(threeDProxyPathForTest); n != 0 {
		t.Fatalf("关闭时不该打上游，实际 %d 次", n)
	}

	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	tools, _ := out["result"].(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		name, _ := raw.(map[string]any)["name"].(string)
		if name == "generate_3d" || name == "get_3d_task" {
			t.Fatalf("3D 关闭时不应列出 %s", name)
		}
	}
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}`)
	instructions, _ := out["result"].(map[string]any)["instructions"].(string)
	if strings.Contains(instructions, "3D") {
		t.Errorf("3D 关闭时 instructions 不该提 3D：%s", instructions)
	}

	// 打开后工具出现、说明补上。
	h.cfg.ThreeDEnabled = true
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	tools, _ = out["result"].(map[string]any)["tools"].([]any)
	var found int
	for _, raw := range tools {
		name, _ := raw.(map[string]any)["name"].(string)
		if name == "generate_3d" || name == "get_3d_task" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("3D 开启后应列出两个工具，实际 %d", found)
	}
}

// 开关打开但没有 ck_t_ 凭证：503，且不发请求（不拿 OAuth token 顶替）。
func TestThreeDNoCredentialReturns503(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) { return 200, threeDSubmitReply("job-1") }}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client(), ThreeDEnabled: true})
	rec := threeDSubmit(t, h, `{"prompt":"a chair"}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "three_d_credential_missing") {
		t.Fatalf("无凭证应 503 three_d_credential_missing，got %d %s", rec.Code, rec.Body)
	}
	if n := f.count(threeDProxyPathForTest); n != 0 {
		t.Fatalf("没有凭证时不该打上游，实际 %d 次", n)
	}
}

func TestThreeDAutoCredentialRESTAndFailureIsolation(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) { return 200, threeDSubmitReply("job-auto") }}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client(), ThreeDEnabled: true})
	var logBuf bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousLog) })
	var fetchCalls int
	h.cfg.Upstream.ThreeDCredential = upstream.NewAutoThreeDCredentials(func(context.Context, *auth.Auth) (string, time.Time, error) {
		fetchCalls++
		return "ck_t_fake_secret", time.Now().Add(time.Hour), nil
	}, nil)
	if rec := threeDSubmit(t, h, `{"prompt":"a chair"}`); rec.Code != 200 {
		t.Fatalf("automatic issue: %d %s", rec.Code, rec.Body)
	}
	if rec := threeDSubmit(t, h, `{"prompt":"another chair"}`); rec.Code != 200 || fetchCalls != 1 {
		t.Fatalf("cached issue calls=%d: %d %s", fetchCalls, rec.Code, rec.Body)
	}
	if f.count(threeDProxyPathForTest) != 2 {
		t.Fatalf("upstream calls=%d", f.count(threeDProxyPathForTest))
	}
	available := len(p.AvailableUIDsForRealm("cn"))
	h.cfg.Upstream.ThreeDCredential = upstream.NewAutoThreeDCredentials(func(context.Context, *auth.Auth) (string, time.Time, error) {
		fetchCalls++
		return "", time.Time{}, fmt.Errorf("%w: fake Bearer oauth-secret", upstream.ErrThreeDNoCredential)
	}, nil)
	rec := threeDSubmit(t, h, `{"prompt":"a chair"}`)
	if rec.Code != http.StatusServiceUnavailable || f.count(threeDProxyPathForTest) != 2 || fetchCalls != 2 {
		t.Fatalf("credential failure code=%d upstream=%d fetch=%d", rec.Code, f.count(threeDProxyPathForTest), fetchCalls)
	}
	if len(p.AvailableUIDsForRealm("cn")) != available {
		t.Fatal("credential failure cooled or rotated the account")
	}
	combined := rec.Body.String() + logBuf.String()
	for _, secret := range []string{"ck_t_fake_secret", "oauth-secret", "Bearer at"} {
		if strings.Contains(combined, secret) {
			t.Errorf("response or log leaked %q", secret)
		}
	}
}

func TestThreeDFailedTaskWithoutCreditDoesNotCharge(t *testing.T) {
	for _, status := range []string{"RUN", "FAIL", "DONE"} {
		f := &videoFake{on: func(videoCall) (int, string) { return 200, threeDTaskReply(status, 0) }}
		rec := usage.New("")
		h := NewHandler(Config{Pool: imagePool(), Upstream: threeDFakeClient(f, map[string]string{"*": "ck_t_test"}), ThreeDEnabled: true, Usage: rec})
		h.threeDLedger.put("job-no-credit", "c1", "cn", "3.1", upstream.ThreeDState{Status: "RUN"})
		response := threeDQuery(t, h, "job-no-credit")
		if response.Code != 200 || strings.Contains(response.Body.String(), `"credit"`) {
			t.Fatalf("%s response=%d %s", status, response.Code, response.Body)
		}
		if got := rec.Snapshot(24, nil).Totals.Credits; got != 0 {
			t.Fatalf("%s fabricated %.2f credit", status, got)
		}
	}
}
func TestThreeDWaitTimeoutReturnsTerminalAndChargesOnce(t *testing.T) {
	old := threeDPollInterval
	threeDPollInterval = time.Millisecond
	t.Cleanup(func() { threeDPollInterval = old })
	f := &videoFake{on: func(c videoCall) (int, string) {
		if strings.Contains(c.Body, `"JobId"`) {
			return 200, threeDTaskReply("DONE", 20)
		}
		return 200, threeDSubmitReply("job-terminal")
	}}
	h := threeDHandler(f, "ck_t_test")
	rec := threeDSubmit(t, h, `{"prompt":"a chair","wait":true,"timeout_seconds":1}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"DONE"`) {
		t.Fatalf("terminal wait code=%d body=%s", rec.Code, rec.Body)
	}
	rec = threeDQuery(t, h, "job-terminal")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"credit":20`) {
		t.Fatalf("terminal cache code=%d body=%s", rec.Code, rec.Body)
	}
}

// 重复查询只记账一次：用量与账号成本都不重复累计。
func TestThreeDRepeatedQueriesChargeOnce(t *testing.T) {
	old := threeDPollInterval
	threeDPollInterval = time.Millisecond
	t.Cleanup(func() { threeDPollInterval = old })
	// 只有第一次查询回 credit，之后不回（模拟上游 usage 只回一次）。
	var polls int
	var mu sync.Mutex
	f := &videoFake{on: func(c videoCall) (int, string) {
		if strings.Contains(c.Body, `"JobId"`) {
			mu.Lock()
			polls++
			first := polls == 1
			mu.Unlock()
			if first {
				return 200, threeDTaskReply("DONE", 20)
			}
			return 200, threeDTaskReply("DONE", 0)
		}
		return 200, threeDSubmitReply("job-once")
	}}
	rec := usage.New("")
	h := NewHandler(Config{
		Pool: imagePool(), Upstream: threeDFakeClient(f, map[string]string{"*": "ck_t_test"}),
		ThreeDEnabled: true, Usage: rec,
	})
	if r := threeDSubmit(t, h, `{"prompt":"a chair","wait":true,"timeout_seconds":1}`); r.Code != 200 {
		t.Fatalf("code=%d body=%s", r.Code, r.Body)
	}
	for i := 0; i < 3; i++ {
		// 清掉终态缓存，逼每次查询都真打上游（上游之后不再回 credit）。
		forgetThreeDState(h, "job-once")
		if r := threeDQuery(t, h, "job-once"); r.Code != 200 {
			t.Fatalf("第 %d 次查询 code=%d body=%s", i+1, r.Code, r.Body)
		}
	}
	if got := rec.Snapshot(24, nil).Totals.Credits; got != 20 {
		t.Fatalf("重复查询应只记一次 20 积分，实际 %.2f", got)
	}
}

// forgetThreeDState 清掉台账里的状态快照（保留记账标记），逼下一次查询真打上游。
func forgetThreeDState(h *Handler, id string) {
	h.threeDLedger.mu.Lock()
	if o, ok := h.threeDLedger.m[id]; ok {
		o.HasState = false
		o.LastQuery = time.Time{}
	}
	h.threeDLedger.mu.Unlock()
}

// 客户端断连：立即停止轮询，不写回执、不记账。
func TestThreeDCancelStopsPollingWithoutCharge(t *testing.T) {
	old := threeDPollInterval
	threeDPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { threeDPollInterval = old })
	f := &videoFake{on: func(c videoCall) (int, string) {
		if strings.Contains(c.Body, `"JobId"`) {
			return 200, threeDTaskReply("PROCESSING", 0)
		}
		return 200, threeDSubmitReply("job-cancel")
	}}
	recUsage := usage.New("")
	h := NewHandler(Config{
		Pool: imagePool(), Upstream: threeDFakeClient(f, map[string]string{"*": "ck_t_test"}),
		ThreeDEnabled: true, Usage: recUsage,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/3d/generations", strings.NewReader(`{"prompt":"a chair","wait":true,"timeout_seconds":600}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.threeDGenerations(rec, r); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后轮询没有停止")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("客户端断连时不该写回执：%s", rec.Body)
	}
	if got := recUsage.Snapshot(24, nil).Totals.Credits; got != 0 {
		t.Errorf("客户端断连时不该记账，实际 %.2f", got)
	}
}

// 并发查同一个 job：上游只被问一次（single-flight），且只记一次账。
func TestThreeDConcurrentQueriesSingleflight(t *testing.T) {
	old := threeDPollInterval
	threeDPollInterval = time.Millisecond
	t.Cleanup(func() { threeDPollInterval = old })
	var polls int
	var mu sync.Mutex
	f := &videoFake{on: func(c videoCall) (int, string) {
		if strings.Contains(c.Body, `"JobId"`) {
			mu.Lock()
			polls++
			mu.Unlock()
			time.Sleep(30 * time.Millisecond) // 拉长在途窗口，让并发请求真的撞上
			return 200, threeDTaskReply("DONE", 20)
		}
		return 200, threeDSubmitReply("job-concurrent")
	}}
	h := threeDHandler(f, "ck_t_test")
	rec := usage.New("")
	h.cfg.Usage = rec
	h.threeDLedger.put("job-concurrent", "c1", "cn", "3.1", upstream.ThreeDState{Status: "PROCESSING"})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			threeDQuery(t, h, "job-concurrent")
		}()
	}
	wg.Wait()

	if polls != 1 {
		t.Fatalf("并发 6 次查询应只打上游 1 次，实际 %d", polls)
	}
	if got := rec.Snapshot(24, nil).Totals.Credits; got != 20 {
		t.Fatalf("并发查询应只记一次 20 积分，实际 %.2f", got)
	}
}

func TestThreeDCachedQueryLogIsSuccess(t *testing.T) {
	withChatLog(t)
	h := threeDHandler(&videoFake{}, "ck_t_test")
	h.threeDLedger.put("job-cached", "c1", "cn", "3.1", upstream.ThreeDState{Status: "DONE", Credit: 20})
	r := httptest.NewRequest("POST", "/v1/3d/tasks", strings.NewReader(`{"job_id":"job-cached"}`))
	r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, &requestTrace{id: "test-cached-query"}))
	output := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		h.threeDTasks(rec, r)
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
	})
	if !strings.Contains(output, "| 200 |") || !strings.Contains(output, "out=success") || strings.Contains(output, "out=http_error") {
		t.Fatalf("cached query log misclassified: %s", output)
	}
}

func TestThreeDAccountRotationOnClientError(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	f := &videoFake{on: func(c videoCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return http.StatusBadRequest, `{"Response":{"Error":{"Code":"InvalidParameter","Message":"bad params"}}}`
		}
		return 200, threeDSubmitReply("job-rot")
	}}
	// 两个 cn 账号：第一个回 400 后应换到第二个。
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{
		Pool: p, Upstream: threeDFakeClient(f, map[string]string{"*": "ck_t_test"}), ThreeDEnabled: true,
	})
	rec := threeDSubmit(t, h, `{"prompt":"a chair"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "job-rot") {
		t.Fatalf("换号后应成功，code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestThreeDRejectsOversizedBody(t *testing.T) {
	h := threeDHandler(&videoFake{}, "ck_t_test")
	rec := threeDSubmit(t, h, `{"prompt":"`+strings.Repeat("x", threeDMaxBody)+`"}`)
	if rec.Code != 413 {
		t.Fatalf("code=%d want 413", rec.Code)
	}
}

func TestThreeDMultiViewImageValidation(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString([]byte("image"))
	for _, req := range []map[string]any{
		{"multi_view_images": "not-an-array"},
		{"multi_view_images": []any{}},
		{"multi_view_images": []any{map[string]any{"ImageBase64": "bad"}}},
	} {
		if _, err := threeDUpstreamBody(req); err == nil {
			t.Fatalf("expected rejection: %v", req)
		}
	}
	body, err := threeDUpstreamBody(map[string]any{"image_base64": valid, "multi_view_images": []any{
		map[string]any{"ViewType": "front", "ViewImageBase64": "data:image/png;base64," + valid},
		map[string]any{"ViewType": "back", "ViewImageUrl": "https://example.com/back.png"},
		"data:image/png;base64," + valid,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if body["MultiViewImages"] == nil {
		t.Fatal("valid multi-view images were dropped")
	}
}

func TestMCPThreeDSubmitAndQuery(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) {
		if strings.Contains(c.Body, `"JobId"`) {
			return 200, threeDTaskReply("DONE", 20)
		}
		return 200, threeDSubmitReply("job-1")
	}}
	h := threeDHandler(f, "ck_t_test")
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"generate_3d","arguments":{"prompt":"a cat"}}}`)
	result, _ := out["result"].(map[string]any)
	sc, _ := result["structuredContent"].(map[string]any)
	if sc["job_id"] != "job-1" || result["isError"] != false {
		t.Fatalf("submit=%v", out)
	}
	_, out = mcpCall(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_3d_task","arguments":{"job_id":"job-1"}}}`)
	result, _ = out["result"].(map[string]any)
	sc, _ = result["structuredContent"].(map[string]any)
	if sc["status"] != "DONE" || sc["result_url"] != "model.glb" {
		t.Fatalf("query=%v", out)
	}
	if files, _ := sc["result_files"].([]any); len(files) != 1 {
		t.Fatalf("result_files=%v", sc)
	}
	// 提交时用了 3.0，回执的 model 不能硬编码成 3.1。
	h2 := threeDHandler(f, "ck_t_test")
	_, out = mcpCall(t, h2, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"generate_3d","arguments":{"prompt":"a cat","model":"3.0"}}}`)
	result, _ = out["result"].(map[string]any)
	sc, _ = result["structuredContent"].(map[string]any)
	if sc["model"] != "3.0" {
		t.Fatalf("回执 model 应沿用提交版本，got %v", sc["model"])
	}
}

func TestMCPThreeDValidation(t *testing.T) {
	h := threeDHandler(&videoFake{}, "ck_t_test")
	_, out := mcpCall(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_3d_task","arguments":{}}}`)
	if out["error"] == nil {
		t.Fatal("missing job_id should be JSON-RPC error")
	}
	rec := threeDSubmit(t, h, `{}`)
	if rec.Code != 400 {
		t.Fatalf("code=%d", rec.Code)
	}
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
}

// —— 重启后有界探测 ——

// 台账为空（重启）但某个账号查得到：探测命中后收养，已终态的任务不再记账。
func TestThreeDProbeAdoptsCompletedTaskWithoutCharge(t *testing.T) {
	f := &videoFake{on: func(c videoCall) (int, string) {
		if c.Host == "global.example" { // 任务属于 global 账号
			return 200, threeDTaskReply("DONE", 20)
		}
		return 404, `{"Response":{"Error":{"Code":"ResourceNotFound","Message":"no such job"}}}`
	}}
	rec := usage.New("")
	h := NewHandler(Config{
		Pool: imagePool(), Upstream: threeDFakeClient(f, map[string]string{"*": "ck_t_test"}),
		ThreeDEnabled: true, Usage: rec,
	})
	if r := threeDQuery(t, h, "job-adopt"); r.Code != 200 {
		t.Fatalf("探测应命中，code=%d body=%s", r.Code, r.Body)
	}
	o, ok := h.threeDLedger.get("job-adopt")
	if !ok || o.UID != "g1" || o.Realm != "global" || !o.Charged {
		t.Fatalf("应收养到 global 账号并标已记账：%+v ok=%v", o, ok)
	}
	if got := rec.Snapshot(24, nil).Totals.Credits; got != 0 {
		t.Fatalf("已终态任务收养时不该再记一次，实际 %.2f", got)
	}
	// 收养后再查走台账，不再碰上游。
	before := f.count(threeDProxyPathForTest)
	threeDQuery(t, h, "job-adopt")
	if n := f.count(threeDProxyPathForTest); n != before {
		t.Errorf("收养后的终态任务不应再打上游：%d → %d", before, n)
	}
}

// 探测过所有候选账号都不是：404，且负缓存生效（第二次不再碰上游）。
func TestThreeDProbeNotFoundThenQuiet(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) {
		return 404, `{"Response":{"Error":{"Code":"ResourceNotFound","Message":"no such job"}}}`
	}}
	h := threeDHandler(f, "ck_t_test")
	if r := threeDQuery(t, h, "ghost"); r.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", r.Code, r.Body)
	}
	first := f.count(threeDProxyPathForTest)
	if first == 0 {
		t.Fatal("首次探测应该问上游")
	}
	if r := threeDQuery(t, h, "ghost"); r.Code != http.StatusNotFound ||
		!strings.Contains(r.Body.String(), "近期刚探测过") {
		t.Fatalf("静默期内的 404 应说明探测被跳过：%d %s", r.Code, r.Body)
	}
	if n := f.count(threeDProxyPathForTest); n != first {
		t.Errorf("静默期内不该再打上游：%d → %d", first, n)
	}
}

// 探测撞 429：立即停手、暂停全局探测，且不让账号进冷却。
func TestThreeDProbe429PausesWithoutCoolingAccounts(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) {
		return http.StatusTooManyRequests, `{"Response":{"Error":{"Code":"RequestLimitExceeded","Message":"too many"}}}`
	}}
	p := imagePool()
	h := NewHandler(Config{
		Pool: p, Upstream: threeDFakeClient(f, map[string]string{"*": "ck_t_test"}),
		ThreeDEnabled: true,
	})
	avail := len(p.AvailableUIDsForRealm("cn"))

	if r := threeDQuery(t, h, "ghost-429"); r.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", r.Code, r.Body)
	}
	if n := f.count(threeDProxyPathForTest); n != 1 {
		t.Fatalf("遇到限流应立即停手，实际探测 %d 次", n)
	}
	if n := len(p.AvailableUIDsForRealm("cn")); n != avail {
		t.Errorf("探测 429 不该让账号进冷却：可用 %d → %d", avail, n)
	}
	threeDQuery(t, h, "ghost-other")
	if n := f.count(threeDProxyPathForTest); n != 1 {
		t.Errorf("暂停期内换 id 仍在探测：共 %d 次", n)
	}
}

// —— 台账淘汰 ——

// 超容量时按创建时间淘汰最旧的一批（不是随机删一个键）。
func TestThreeDTaskStoreEvictsOldest(t *testing.T) {
	s := newThreeDTaskStore()
	base := time.Now().Add(-time.Hour)
	s.mu.Lock()
	for i := 0; i < threeDTaskMax; i++ {
		s.m[id(i)] = &threeDTaskOwner{UID: "c1", Created: base.Add(time.Duration(i) * time.Second)}
	}
	s.mu.Unlock()

	s.put("newest", "c1", "cn", "3.1", upstream.ThreeDState{Status: "SUBMITTED"})

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= threeDTaskMax {
		t.Fatalf("超容量后应淘汰到 3/4（%d），实际 %d", threeDTaskMax*3/4, len(s.m))
	}
	for i := 0; i < 10; i++ { // 最旧的 10 个必须没了
		if _, ok := s.m[id(i)]; ok {
			t.Fatalf("最旧的 %s 应被淘汰", id(i))
		}
	}
	if _, ok := s.m[id(threeDTaskMax-1)]; !ok {
		t.Error("最新的条目不该被淘汰")
	}
	if _, ok := s.m["newest"]; !ok {
		t.Error("刚插入的条目不该被淘汰")
	}
}

func id(i int) string { return fmt.Sprintf("t%04d", i) }

// 超龄条目在容量内也按 TTL 清掉。
func TestThreeDTaskStoreEvictsExpired(t *testing.T) {
	s := newThreeDTaskStore()
	s.mu.Lock()
	s.m["old"] = &threeDTaskOwner{UID: "c1", Created: time.Now().Add(-threeDTaskTTL - time.Minute)}
	s.mu.Unlock()
	s.put("fresh", "c1", "cn", "3.1", upstream.ThreeDState{})
	if _, ok := s.get("old"); ok {
		t.Fatal("超龄条目应被清掉")
	}
	if _, ok := s.get("fresh"); !ok {
		t.Fatal("新条目不该被清掉")
	}
}
