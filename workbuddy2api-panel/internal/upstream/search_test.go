package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// searchStub 记录落点/头/体，并按脚本回状态与原文。
type searchStub struct {
	path   string
	host   string
	body   map[string]any
	header http.Header
	status int
	resp   string
	hits   int
}

func (s *searchStub) client() *Client {
	return &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			s.hits++
			s.path, s.host, s.header = r.URL.Path, r.URL.Host, r.Header.Clone()
			s.body = map[string]any{}
			_ = json.Unmarshal(raw, &s.body)
			return &http.Response{
				StatusCode: s.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(s.resp)),
			}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

const searchOK = `{"query":"电梯 维保","type":"text2text","provider":"0",` +
	`"results":[{"title":"条例","url":"https://x.example/1","snippet":"..."}],` +
	`"total_results":5,"response_time_ms":631}`

func cnAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
}

// TestAgentToolSearchShape 检索落点、头族、请求体归一与原文透传。
func TestAgentToolSearchShape(t *testing.T) {
	s := &searchStub{status: 200, resp: searchOK}
	data, err := s.client().Search(context.Background(), cnAuth(), "电梯 维保", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.path != "/agenttool/v1/search" || s.host != "cn.example" {
		t.Fatalf("落点 %s%s want /agenttool/v1/search @ cn.example", s.host, s.path)
	}
	// 渠道校验三件套恒发（与 chat 同源头族）：缺任一个上游按 11-128 拒。
	// 只断言**必发**的两个：RequestID（轮级）+ MessageID（消息级）。
	// X-Conversation-ID 按本仓口径「body 提取，空则不发（不伪造）」，零值下确实为空。
	for _, k := range []string{"X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
		if s.header.Get(k) == "" {
			t.Fatalf("缺会话头 %s（零值 ChatMeta 必须自造，不得留空）", k)
		}
	}
	if s.body["type"] != "text2text" {
		t.Fatalf("type=%v want text2text（唯一合法值）", s.body["type"])
	}
	if s.body["max_results"] != float64(WebSearchDefaultMaxResults) {
		t.Fatalf("max_results=%v want 默认 %d", s.body["max_results"], WebSearchDefaultMaxResults)
	}
	if s.body["query"] != "电梯 维保" {
		t.Fatalf("query=%v", s.body["query"])
	}
	// provider 是响应字段、不可入请求（带上即 400/15003）：漏进来就是线上事故。
	if _, ok := s.body["provider"]; ok {
		t.Fatal("请求体不得带 provider 字段")
	}
	var got SearchResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Results[0].URL != "https://x.example/1" || got.TotalResults != 5 {
		t.Fatalf("原文透传不符: %s", data)
	}
}

// TestAgentToolErrors ≥400 与 200-带业务码都要变成 *Error（后者防「把错误当结果透传」）。
func TestAgentToolErrors(t *testing.T) {
	s := &searchStub{status: 400, resp: `{"code":15003,"msg":"unsupported search provider: 0"}`}
	_, err := s.client().Search(context.Background(), cnAuth(), "x", 5)
	var ue *Error
	if err == nil || !asError(err, &ue) {
		t.Fatalf("400 必须回 *Error，得到 %v", err)
	}
	if ue.Status != 400 || !strings.Contains(ue.Msg, "15003") {
		t.Fatalf("错误信封不符: %+v", ue)
	}

	s2 := &searchStub{status: 200, resp: `{"code":15013,"msg":"url is required"}`}
	_, err = s2.client().WebFetch(context.Background(), cnAuth(), "")
	if err == nil || !asError(err, &ue) {
		t.Fatalf("200 + code=15013 必须回 *Error，得到 %v", err)
	}
	if s2.path != "/agenttool/v1/webfetch" || s2.body["url"] != "" {
		t.Fatalf("webfetch 落点/体不符: %s %v", s2.path, s2.body)
	}
}

// asError 本地避免再引 errors 包名与现有测试冲突。
func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
