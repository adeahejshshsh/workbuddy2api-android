package panel

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
)

// 面板挂在 CDN 后时，固定 URL 的 app.js 会被边缘缓存数小时：升级二进制后
// 「新页面 + 旧 app.js」→ 新按钮没绑事件，用户侧就是「点了没反应」。
// 资源引用必须带版本串（misakano/qcgzxw `1ab95fad`）。
func TestVersionedIndexAddsAssetVersion(t *testing.T) {
	p := &Panel{}
	p.cfg.Version = "1.35.1-202610221703"

	body, etag := p.versionedIndex()
	if !strings.Contains(string(body), `src="app.js?v=1.35.1-202610221703"`) {
		t.Fatalf("资源引用没带版本串: %s", firstLineWith(string(body), "app.js"))
	}
	// ETag 必须跟着实际字节走，否则 304 会与另一份内容错配。
	if etag != contentETag(body) {
		t.Fatal("ETag 不是按版本化后的字节算的")
	}
	if etag == indexHTMLETag {
		t.Fatal("ETag 不该等于未版本化页面的 ETag")
	}

	// 未构建版本（go run）时不带版本串，避免本地开发把旧资源钉死。
	p2 := &Panel{}
	p2.cfg.Version = "dev"
	body2, etag2 := p2.versionedIndex()
	if strings.Contains(string(body2), "app.js?v=") {
		t.Fatal("dev 版本不该加版本串")
	}
	if etag2 != indexHTMLETag {
		t.Fatal("dev 版本应沿用原 ETag")
	}
}

// 端到端：GET /panel/ 返回的页面里资源引用带版本串。
func TestPanelIndexServedWithAssetVersion(t *testing.T) {
	p := New(Config{Version: "1.35.1-202610221703", APIKey: "test-key"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "app.js?v=1.35.1-202610221703") {
		t.Fatal("服务端返回的页面没带资源版本串")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control=%q want no-cache（有版本串也要回源校验）", got)
	}
}

func firstLineWith(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return "(未找到)"
}

// 积分流水接口：无账本时返回空列表（不能 500），有账本时按 uid/limit 过滤。
func TestCreditHistoryAPI(t *testing.T) {
	p := New(Config{Version: "test"}) // 不设 APIKey = 不鉴权（本用例只测聚合逻辑）
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/credit_history", nil))
	if rec.Code != 200 {
		t.Fatalf("无账本时 code=%d body=%s", rec.Code, rec.Body)
	}

	l := credithist.New("", 100)
	l.Observe("u1", 100)
	l.Observe("u1", 150)
	l.Observe("u2", 10)
	l.Observe("u2", 20)
	p2 := New(Config{Version: "test", CreditHistory: l})
	rec2 := httptest.NewRecorder()
	p2.ServeHTTP(rec2, httptest.NewRequest("GET", "/panel/api/credit_history?uid=u1&limit=5", nil))
	if rec2.Code != 200 {
		t.Fatalf("code=%d body=%s", rec2.Code, rec2.Body)
	}
	body := rec2.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) {
		t.Fatalf("缺 u1 记录: %s", body)
	}
	if strings.Contains(body, `"uid":"u2"`) {
		t.Fatalf("uid 过滤没生效: %s", body)
	}
}
