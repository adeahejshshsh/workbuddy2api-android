package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// agenticStub 按给定分隔符回一段 SSE（默认 LF；CRLF 变体用来钉住帧分隔的容错）。
type agenticStub struct {
	path, body string
	status     int
	raw        string
	sep        string
}

func (s *agenticStub) client() *Client {
	sep := s.sep
	if sep == "" {
		sep = "\n"
	}
	return &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			s.path, s.body = r.URL.Path, string(raw)
			body := strings.ReplaceAll(s.raw, "\n", sep)
			return &http.Response{StatusCode: s.status,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

// agenticSSE 中间事件 + done 帧（截取线上实测的真实形状，正文缩短）。
const agenticSSE = "event: iteration\ndata: {\"round\":1}\n\n" +
	"event: synthesis_delta\ndata: {\"delta\":\"部分\"}\n\n" +
	"event: done\ndata: {\"latency_ms\":65108,\"partial\":false,\"partial_reason\":\"\"," +
	"\"synthesis\":{\"content\":\"结论：桂市监规〔2025〕7号自 2026-01-01 施行\",\"inline_content\":\"结论\"}," +
	"\"results\":[{\"id\":\"yuanbao_1\",\"content\":\"整页正文\"}]," +
	"\"statuses\":[{\"source\":\"websearch\",\"status\":\"OK\",\"latency_ms\":762,\"result_count\":9}]," +
	"\"token_usage\":{\"prompt_tokens\":47786,\"completion_tokens\":1656}}\n\n"

// TestAgenticSearchDoneFrame 落点/体/原文：done 帧的 data 原样返回（一个字段都不改写）。
func TestAgenticSearchDoneFrame(t *testing.T) {
	s := &agenticStub{status: 200, raw: agenticSSE}
	got, err := s.client().AgenticSearch(context.Background(), cnAuth(), "玉林 电梯 维保 信用等级")
	if err != nil {
		t.Fatal(err)
	}
	if s.path != "/agenttool/v1/agentic_search" {
		t.Fatalf("path=%s", s.path)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(s.body), &sent)
	if sent["query"] != "玉林 电梯 维保 信用等级" || sent["stream"] != true ||
		sent["biz_via"] != "public" || sent["search_mode"] != float64(agenticSearchMode) {
		t.Fatalf("请求体不符: %v", sent)
	}
	var done struct {
		Partial   bool `json:"partial"`
		Synthesis struct {
			Content string `json:"content"`
		} `json:"synthesis"`
		Results []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(got, &done); err != nil {
		t.Fatalf("done 帧不是完整 JSON: %v (%s)", err, got)
	}
	if done.Partial || !strings.Contains(done.Synthesis.Content, "桂市监规") {
		t.Fatalf("综述未透传: %s", got)
	}
	if len(done.Results) != 1 || done.Results[0].Content != "整页正文" {
		t.Fatalf("results（整页正文）未透传: %s", got)
	}
}

// TestAgenticSearchCRLFFrames 帧分隔容错：CRLF 流也必须切得出 done（只找 "\n\n" 会漏）。
func TestAgenticSearchCRLFFrames(t *testing.T) {
	s := &agenticStub{status: 200, raw: agenticSSE, sep: "\r\n"}
	got, err := s.client().AgenticSearch(context.Background(), cnAuth(), "x")
	if err != nil {
		t.Fatalf("CRLF 流取 done 失败: %v", err)
	}
	if !strings.Contains(string(got), "桂市监规") {
		t.Fatalf("CRLF 流内容不符: %s", got)
	}
}

// TestAgenticSearchEnterpriseBizVia 企业号 biz_via=internal（官方客户端取法）。
func TestAgenticSearchEnterpriseBizVia(t *testing.T) {
	s := &agenticStub{status: 200, raw: agenticSSE}
	a := cnAuth()
	a.EnterpriseID = "ent-1"
	if _, err := s.client().AgenticSearch(context.Background(), a, "x"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.body, `"biz_via":"internal"`) {
		t.Fatalf("企业号应走 internal: %s", s.body)
	}
}

// TestAgenticSearchNoDoneIsError 200 但没有 done 帧 → 报错，不返回半截内容。
func TestAgenticSearchNoDoneIsError(t *testing.T) {
	s := &agenticStub{status: 200, raw: "event: iteration\ndata: {\"round\":1}\n\n"}
	if _, err := s.client().AgenticSearch(context.Background(), cnAuth(), "x"); err == nil {
		t.Fatal("没有 done 帧应报错")
	}
	// ≥400 走 Classify：这里顺带钉住错误信封。
	s2 := &agenticStub{status: 400, raw: `{"code":15003,"msg":"unsupported search provider: 0"}`}
	_, err := s2.client().AgenticSearch(context.Background(), cnAuth(), "x")
	var ue *Error
	if err == nil || !asError(err, &ue) || ue.Status != 400 {
		t.Fatalf("400 应回 *Error: %v", err)
	}
}
