package server

// /v1/embeddings：把上游 {code,msg,model,data[{embedding}]} 归一成 OpenAI 形状，
// 并钉住「模型不存在 → 404 且不换号」（注册表按模型不按账号，换号无意义）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestEmbeddingsEndpoint(t *testing.T) {
	const upstreamBody = `{"code":0,"msg":"ok","model":"bge-m3","data":[{"embedding":[0.1,0.2]},{"embedding":[0.3,0.4]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`
	f := &searchFake{status: 200, body: upstreamBody}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client(), APIKey: "k", EmbeddingsEnabled: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"bge-m3","input":["a","b"]}`))
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	// OpenAI 形状：object=list + data[i].object=embedding + index 补齐
	if got["object"] != "list" {
		t.Errorf("object=%v want list", got["object"])
	}
	data, _ := got["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data 长度=%d want 2 (%s)", len(data), rec.Body.String())
	}
	first, _ := data[0].(map[string]any)
	if first["object"] != "embedding" {
		t.Errorf("data[0].object=%v want embedding", first["object"])
	}
	if _, ok := first["index"]; !ok {
		t.Errorf("data[0] 缺 index（上游不返回，网关须补齐）")
	}
	if f.paths[0] != "/v2/embeddings" {
		t.Fatalf("落点应为 /v2/embeddings: %v", f.paths)
	}
}

// 模型不存在（上游 11351）→ 404 model_not_found，且**不换号**（注册表按模型不按账号）。
func TestEmbeddingsModelNotFoundNoRotation(t *testing.T) {
	f := &searchFake{status: 500, body: `{"code":11351,"msg":"embedding model [x] not found"}`}
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "c2", AccessToken: "at2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	h := NewHandler(Config{Pool: p, Upstream: f.client(), APIKey: "k", EmbeddingsEnabled: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"nope","input":"x"}`))
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 (%s)", rec.Code, rec.Body.String())
	}
	if len(f.paths) != 1 {
		t.Fatalf("模型不存在不该换号重试，实际打了 %d 次", len(f.paths))
	}
}
