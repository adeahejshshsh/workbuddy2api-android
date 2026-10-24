package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// searchFake 记录检索落点/体，并按脚本回状态与原文。
type searchFake struct {
	paths  []string
	hosts  []string
	bodies []string
	status int
	body   string
}

func (f *searchFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			f.paths = append(f.paths, r.URL.Path)
			f.hosts = append(f.hosts, r.URL.Host)
			f.bodies = append(f.bodies, string(raw))
			return &http.Response{
				StatusCode: f.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(f.body)),
			}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

const searchOKBody = `{"query":"玉林 电梯 维保","type":"text2text","provider":"0",` +
	`"results":[{"title":"新规","url":"https://x.example/1","snippet":"..."}],` +
	`"total_results":5,"response_time_ms":631}`

// dshUA DSH（DeepSeek Harness）的实测 User-Agent；兼容层只对含
// "deepseek-harness" 的请求启用。
const dshUA = "deepseek-harness/0.2.1-rc.2 (+https://github.com/deepseek-ai/deepseek-harness)"

// TestSearchEndpoint POST /v1/search：原文透传、缺 query 400、请求级 400 不轮转。
func TestSearchEndpoint(t *testing.T) {
	f := &searchFake{status: 200, body: searchOKBody}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f.client()})
	postUA := func(body, ua string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/search", strings.NewReader(body))
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// 默认按 DSH 客户端发（兼容层的目标读者）。
	post := func(body string) *httptest.ResponseRecorder { return postUA(body, dshUA) }

	// ① 成功：上游字段语义不变（仅补 DeepSeek Search API 需要的 date:null），落点在 CN 域
	rec := post(`{"query":"玉林 电梯 维保","max_results":5}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Query        string           `json:"query"`
		Type         string           `json:"type"`
		Provider     string           `json:"provider"`
		Results      []map[string]any `json:"results"`
		TotalResults int              `json:"total_results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if got.Query != "玉林 电梯 维保" || got.Type != "text2text" || got.Provider != "0" || got.TotalResults != 5 {
		t.Fatalf("上游字段透传不符: %+v", got)
	}
	if len(got.Results) != 1 || got.Results[0]["url"] != "https://x.example/1" ||
		got.Results[0]["title"] != "新规" || got.Results[0]["snippet"] != "..." {
		t.Fatalf("结果项透传不符: %+v", got.Results)
	}
	// date 必须存在且为 null：DSH 的 deepseek-search-api 用 `item.date !== null &&
	// typeof item.date !== 'string'` 校验，缺字段（undefined）会直接判 invalidResponse。
	if v, ok := got.Results[0]["date"]; !ok || v != nil {
		t.Fatalf("date 需为显式 null，得到 ok=%v v=%v", ok, v)
	}
	if f.paths[0] != "/agenttool/v1/search" || f.hosts[0] != "cn.example" {
		t.Fatalf("落点 %s%s", f.hosts[0], f.paths[0])
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	if sent["type"] != "text2text" || sent["max_results"] != float64(5) {
		t.Fatalf("请求体归一不符: %v", sent)
	}

	// ①b DeepSeek 形状只发 count（不发 max_results）：要回落生效，不能静默按默认 5 条
	f3 := &searchFake{status: 200, body: searchOKBody}
	h3 := NewHandler(Config{Pool: p, Upstream: f3.client()})
	req3 := httptest.NewRequest("POST", "/v1/search",
		strings.NewReader(`{"query":"x","count":3,"need_content":false}`))
	req3.Header.Set("User-Agent", dshUA)
	rec = httptest.NewRecorder()
	h3.ServeHTTP(rec, req3)
	if rec.Code != 200 {
		t.Fatalf("count 请求 code=%d body=%s", rec.Code, rec.Body.String())
	}
	var sentCount map[string]any
	_ = json.Unmarshal([]byte(f3.bodies[0]), &sentCount)
	if sentCount["max_results"] != float64(3) {
		t.Fatalf("count 未回落为 max_results: %v", sentCount)
	}

	// ①c 非 DSH 客户端：兼容层必须完全不介入——响应逐字节等于上游原文（不补 date、
	// 不重排字段），count 也不参与条数（仍由上游默认 5 条兜底）。
	f4 := &searchFake{status: 200, body: searchOKBody}
	h4 := NewHandler(Config{Pool: p, Upstream: f4.client()})
	req4 := httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x","count":3}`))
	req4.Header.Set("User-Agent", "curl/8.23.0")
	rec = httptest.NewRecorder()
	h4.ServeHTTP(rec, req4)
	if rec.Code != 200 {
		t.Fatalf("非 DSH 请求 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != searchOKBody {
		t.Fatalf("非 DSH 响应应逐字节透传，得到 %s", got)
	}
	var sentPlain map[string]any
	_ = json.Unmarshal([]byte(f4.bodies[0]), &sentPlain)
	if sentPlain["max_results"] != float64(5) {
		t.Fatalf("非 DSH 的 count 不应回落（应走上游默认 5）: %v", sentPlain)
	}

	// ② 缺 query：400 且不打上游（少一次白烧）
	before := len(f.paths)
	if rec := post(`{"query":"  "}`); rec.Code != 400 {
		t.Fatalf("空 query 应 400，得到 %d", rec.Code)
	}
	if len(f.paths) != before {
		t.Fatal("空 query 不应发出上游请求")
	}

	// ③ 请求级 400（15003）：原文透传、只打 1 次上游（不轮转烧第二个号）
	f2 := &searchFake{status: 400, body: `{"code":15003,"msg":"unsupported search provider: 0"}`}
	h2 := NewHandler(Config{Pool: p, Upstream: f2.client()})
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "15003") {
		t.Fatalf("请求级 400 应原文透传，得到 %d %s", rec.Code, rec.Body.String())
	}
	if len(f2.paths) != 1 {
		t.Fatalf("请求级错误应 fail-fast 不轮转，实际打了 %d 次", len(f2.paths))
	}

	// ④ 无可用账号：503 no_account（不是含糊的 502）
	empty := NewHandler(Config{Pool: testPoolWith(), Upstream: f.client()})
	rec = httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(`{"query":"x"}`)))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "no_account") {
		t.Fatalf("空池应 503 no_account，得到 %d %s", rec.Code, rec.Body.String())
	}
}

// 说明：上游的 deep:true（agentic_search 综述式检索）本仓未移植 —— 它依赖
// AgenticSearch 客户端方法与 40~66s 的长请求语义，属独立特性。此处删掉对应用例，
// count 兼容与客户端识别两个用例保留。

// TestCompatDeepSeekSearch 只补 date:null，其余字段值原样保留；坏输入不篡改。
//
// DSH（deepseek-harness）的 deepseek-search-api provider 对响应校验很硬：
// results[].date 必须是 null 或 string，字段缺失（undefined）直接判
// invalidResponse —— 线上表现为 “DeepSeek returned an invalid response body”。
func TestCompatDeepSeekSearch(t *testing.T) {
	in := `{"query":"q","results":[{"title":"a","url":"https://x/1","snippet":"s"},` +
		`{"title":"b","url":"https://x/2","snippet":"","date":"2026-01-02"},` +
		`{"url":"https://x/3","snippet":null}]}`
	var out map[string]any
	if err := json.Unmarshal(compatDeepSeekSearch([]byte(in)), &out); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	items, _ := out["results"].([]any)
	if len(items) != 3 {
		t.Fatalf("results 数量变了: %v", out)
	}
	first, _ := items[0].(map[string]any)
	if v, ok := first["date"]; !ok || v != nil {
		t.Fatalf("缺 date 的项应补 null: ok=%v v=%v", ok, v)
	}
	if first["url"] != "https://x/1" || first["snippet"] != "s" || first["title"] != "a" {
		t.Fatalf("其余字段应原样保留: %v", first)
	}
	second, _ := items[1].(map[string]any)
	if second["date"] != "2026-01-02" {
		t.Fatalf("已有 date 应原样保留: %v", second)
	}
	// 缺 title / snippet（或为 null）时补空串：DSH 对两者也要求 string
	third, _ := items[2].(map[string]any)
	if third["title"] != "" || third["snippet"] != "" || third["date"] != nil {
		t.Fatalf("缺 title/snippet 的项应补空串、date 补 null: %v", third)
	}

	// 无法安全改写时原样返回，绝不把好响应改坏
	for _, raw := range []string{`not json`, `{"query":"q"}`, `{"results":[]}`, `{"results":"x"}`, ``} {
		if got := string(compatDeepSeekSearch([]byte(raw))); got != raw {
			t.Fatalf("应原样返回 %q，得到 %q", raw, got)
		}
	}
}

// TestDeepSeekSearchClientDetection 兼容层只认 DSH 的 UA（窄子串、大小写不敏感），
// 其余客户端（含空 UA）一律走原文透传。
func TestDeepSeekSearchClientDetection(t *testing.T) {
	for _, ua := range []string{
		dshUA,
		"DeepSeek-Harness/0.3.0",
		"deepseek-harness",
	} {
		if !deepSeekSearchClient(ua) {
			t.Errorf("UA %q 应识别为 DSH", ua)
		}
	}
	for _, ua := range []string{
		"",
		"curl/8.23.0",
		"python-requests/2.32",
		"deepseek-search-api/1.0",
		"CodeBuddy/2.63.2",
	} {
		if deepSeekSearchClient(ua) {
			t.Errorf("UA %q 不应识别为 DSH", ua)
		}
	}
}
