// OpenAI Images 规范里本网关要自己补的两处：response_format=b64_json（上游无视，
// 只回直链）与 n>1（上游一次只出一张，静默少给比拒绝更糟）。
package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postImage(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)))
	return rec
}

func TestImagesB64JSONConversion(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n-fake-png-bytes")
	allowMediaLoopback(t)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer cdn.Close()

	f := &imageFake{status: 200, body: `{"code":0,"msg":"OK","data":{"created":1791104365,"data":[{"url":"` +
		cdn.URL + `/img.png"}],"usage":{"credit":5.71}}}`}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	rec := postImage(t, h, `{"model":"cn:hunyuan-image-alpha","prompt":"一只猫","response_format":"b64_json"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("data=%v", out.Data)
	}
	if _, hasURL := out.Data[0]["url"]; hasURL {
		t.Fatalf("给了 b64_json 就不该再给 url：%v", out.Data[0])
	}
	got, _ := out.Data[0]["b64_json"].(string)
	if raw, _ := base64.StdEncoding.DecodeString(got); string(raw) != string(png) {
		t.Fatalf("b64_json 内容不对：%q", got)
	}
}

// 取图失败不能把成品弄丢：url 原样保留（图已经出了、也已经扣费）。
func TestImagesB64JSONKeepsURLOnFetchFailure(t *testing.T) {
	f := &imageFake{status: 200, body: `{"code":0,"msg":"OK","data":{"created":1,"data":[{"url":"https://127.0.0.1:1/gone.png"}],` +
		`"usage":{"credit":5.71}}}`}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	rec := postImage(t, h, `{"model":"cn:hunyuan-image-alpha","prompt":"一只猫","response_format":"b64_json"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Data[0]["url"] == nil {
		t.Fatalf("取图失败时应保留 url：%v", out.Data[0])
	}
	if out.Data[0]["b64_json"] != nil {
		t.Fatalf("取图失败不该有 b64_json：%v", out.Data[0])
	}
}

// n>1 明确拒绝，且不打上游（上游一次只出一张）。
func TestImagesRejectsNMoreThanOne(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	rec := postImage(t, h, `{"model":"cn:hunyuan-image-alpha","prompt":"一只猫","n":2}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if len(f.paths) != 0 {
		t.Fatalf("n>1 不该打上游，实际 %d 次", len(f.paths))
	}
	if rec := postImage(t, h, `{"model":"cn:hunyuan-image-alpha","prompt":"一只猫","n":1}`); rec.Code != http.StatusOK {
		t.Fatalf("n=1 应正常出图，code=%d body=%s", rec.Code, rec.Body)
	}
}
