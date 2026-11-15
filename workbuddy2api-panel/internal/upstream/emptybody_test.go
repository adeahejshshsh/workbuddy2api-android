package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 200 + 空 body 是成功（审查 F-08）：调用方普遍 `data, err := doJSON(); json.Unmarshal(data, &out)`，
// 返回空 raw 会让 Unmarshal 报 unexpected end of JSON input，把成功记成失败。
func TestDoJSONEmptyBodyIsUnmarshalable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 空 body
	}))
	defer srv.Close()

	c := New()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.doJSON(req)
	if err != nil {
		t.Fatalf("空体成功不该报错: %v", err)
	}
	var out struct{ A int }
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("空体成功必须能 Unmarshal，实际: %v（raw=%q）", err, string(data))
	}
}
