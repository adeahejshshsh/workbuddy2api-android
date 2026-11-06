// Responses 托管 image_generation 的探测参数、积分闸与出图管道（与 /v1/images 对齐）。
package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// pipeFake 可编程的上游：chat 首轮回「要出图」，之后正常答；出图按次数回放 imgResp。
type pipeFake struct {
	chatCalls atomic.Int32
	imgCalls  atomic.Int32
	// imgResp 返回第 n 次（从 1 起）出图的 (HTTP 状态, body, 传输层错误)。
	imgResp func(n int) (int, string, error)
	// onImage 每次出图请求到达时调用（用来模拟客户端此刻断连）。
	onImage func()
	// withLog 接上请求台账与来源采集（流水行才带 rid/src）。
	withLog bool
}

func (f *pipeFake) handler(accounts ...string) *Handler {
	cl := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			_, _ = io.ReadAll(r.Body)
			if strings.HasPrefix(r.URL.Path, "/v2/images") {
				n := int(f.imgCalls.Add(1))
				if f.onImage != nil {
					f.onImage()
				}
				status, body, err := f.imgResp(n)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: status, Header: jsonHdr(),
					Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			body := sseOK
			if f.chatCalls.Add(1) == 1 {
				body = imageToolCallStream()
			}
			return &http.Response{StatusCode: 200, Header: sseHdr(), Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	var auths []*auth.Auth
	for _, uid := range accounts {
		auths = append(auths, &auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	}
	cfg := Config{Pool: testPoolWith(auths...), Upstream: cl}
	if f.withLog {
		cfg.RequestLog, cfg.RecordClientInfo = reqlog.New(reqlog.Config{}), true
	}
	return NewHandler(cfg)
}

func postImageToolReq(ctx context.Context, h *Handler, stream bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"glm-5.2","input":"画一张图","stream":`+strconv.FormatBool(stream)+
			`,"tools":[{"type":"image_generation"}]}`)).WithContext(ctx)
	h.ServeHTTP(rec, req)
	return rec
}

// 缺陷 1：探测函数的参数名随工具走（image_generation 写 prompt，web_search 写 query），
// schema 不再与描述矛盾。
func TestToolProbeFunctionParamName(t *testing.T) {
	for name, want := range map[string]string{imageToolName: "prompt", webSearchToolName: "query"} {
		fn, _ := toolProbeFunction(name)["function"].(map[string]any)
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		req, _ := params["required"].([]string)
		if _, ok := props[want]; !ok || len(props) != 1 || len(req) != 1 || req[0] != want {
			t.Errorf("%s 的探测参数应只有必填的 %s：props=%v required=%v", name, want, props, req)
		}
	}
}

// 缺陷 2：积分闸在探测与出图之前过——被闸拦下的请求不能先出图再被拒（图白扣）。
// 拒绝形状与 chatCompletions 一致：非流式同一状态码，流式折成 response.failed。
func TestResponsesImageToolBlockedByBudgetGate(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			f := &pipeFake{imgResp: func(int) (int, string, error) { return 200, `{}`, nil }}
			h := f.handler("c1")
			h.SetBudgetLimit(1)
			h.budget.add(5, true)

			rec := postImageToolReq(context.Background(), h, stream)
			if f.chatCalls.Load() != 0 || f.imgCalls.Load() != 0 {
				t.Fatalf("闸拒绝后不该有上游调用：对话 %d 次，出图 %d 次", f.chatCalls.Load(), f.imgCalls.Load())
			}
			if stream {
				if !strings.Contains(rec.Body.String(), "response.failed") {
					t.Fatalf("流式拒绝应折成 response.failed：code=%d body=%s", rec.Code, rec.Body)
				}
			} else if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("code=%d want 429（同 chatCompletions）body=%s", rec.Code, rec.Body)
			}
		})
	}
}

// 前置闸放行之后，出图入账把累计推过日上限：对话一步不能再按新累计把请求拒掉，否则图扣了钱却交不出去。
// 日上限 10、已用 5，出图 5.71 之后累计 10.71，对话一步照常跑、整个请求 200。
func TestResponsesImageToolNotRejectedAfterImageCrossesLimit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			f := &pipeFake{imgResp: func(int) (int, string, error) { return 200, imageOK, nil }}
			h := f.handler("c1")
			h.SetBudgetLimit(10)
			h.budget.add(5, true)

			rec := postImageToolReq(context.Background(), h, stream)
			if f.imgCalls.Load() != 1 || f.chatCalls.Load() != 2 {
				t.Fatalf("出图后对话一步应照常执行：出图 %d 次，对话 %d 次", f.imgCalls.Load(), f.chatCalls.Load())
			}
			if rec.Code != 200 || strings.Contains(rec.Body.String(), "response.failed") {
				t.Fatalf("已放行的请求不该在对话一步被拒：code=%d body=%.300s", rec.Code, rec.Body)
			}
		})
	}
}

// 流式请求在出图期间断连：回 499、什么都不写，不能补一条 200 + response.failed 被记成成功请求。
func TestResponsesImageToolStreamDisconnectIs499(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &pipeFake{imgResp: func(int) (int, string, error) { return 200, imageOK, nil }, onImage: cancel}
	h := f.handler("c1")
	rec := postImageToolReq(ctx, h, true)
	if rec.Code != 499 || strings.Contains(rec.Body.String(), "response.failed") {
		t.Fatalf("流式断连应 499 且不写回程：code=%d body=%s", rec.Code, rec.Body)
	}
}

// 缺陷 3b：每次尝试前查 ctx——客户端断连后不再起新的出图尝试；且 Responses 入口随后
// 不再跑对话（回 499）。
func TestImageGenerateStopsAfterDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &pipeFake{
		imgResp: func(int) (int, string, error) { return 500, `{"code":500,"msg":"boom"}`, nil },
		onImage: cancel,
	}
	h := f.handler("c1", "c2", "c3")
	rec := postImageToolReq(ctx, h, false)
	if n := f.imgCalls.Load(); n != 1 {
		t.Fatalf("断连后不该再起出图尝试：出图 %d 次", n)
	}
	if f.chatCalls.Load() != 1 || rec.Code != 499 {
		t.Fatalf("断连后不该再跑对话：对话 %d 次（只有探测那一次）code=%d", f.chatCalls.Load(), rec.Code)
	}
}

// 缺陷 4：出图流水行带该 Responses 请求的 rid 与来源（IP/UA），能与对话行对上；
// 请求级错误（11101 参数错）不换号、流水行记 400。
func TestImageGenerateRowCarriesRequestIdentity(t *testing.T) {
	withChatLog(t)
	f := &pipeFake{withLog: true, imgResp: func(int) (int, string, error) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, nil
	}}
	h := f.handler("c1", "c2")
	var rid string
	out := captureStdout(t, func() {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
			`{"model":"glm-5.2","input":"画一张图","stream":false,"tools":[{"type":"image_generation"}]}`))
		req.RemoteAddr = "203.0.113.7:4321"
		req.Header.Set("User-Agent", "codex-cli/1.0")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		rid = rec.Header().Get("X-Request-Id")
	})
	var row string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "hunyuan-image-alpha") {
			row = l
		}
	}
	if rid == "" || row == "" {
		t.Fatalf("没找到出图流水行 rid=%q：\n%s", rid, out)
	}
	if !strings.Contains(row, "rid="+rid) || !strings.Contains(row, "src=203.0.113.7") {
		t.Errorf("出图流水行应带请求 rid 与来源：%s", row)
	}
	if !strings.Contains(row, "| 400 |") || f.imgCalls.Load() != 1 {
		t.Errorf("请求级错误应夹到 400 且不换号：出图 %d 次 行=%s", f.imgCalls.Load(), row)
	}
}
