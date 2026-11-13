package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPanelRootRedirect 根路径必须 302 到 /panel/，并保留查询串。
// 不能直接把面板 HTML 挂在 "/"：HTML 里的 theme.js / app.js 是同目录相对路径，
// 挂根之后会解析成 /theme.js → 404，面板白屏。
func TestPanelRootRedirect(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"/", "/panel/"},
		{"/?minis=1", "/panel/?minis=1"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.raw, nil)
		rec := httptest.NewRecorder()
		panelRootRedirect(rec, req)

		if rec.Code != http.StatusFound {
			t.Errorf("%s 状态码 = %d，期望 302", c.raw, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != c.want {
			t.Errorf("%s Location = %q，期望 %q", c.raw, got, c.want)
		}
	}
}
