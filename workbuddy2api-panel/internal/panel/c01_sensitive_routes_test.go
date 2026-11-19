// c01_sensitive_routes_test.go 面板敏感接口判定的**结构性**回归钉（C-01 / W3-01 / W3-21）。
//
// 为什么不能用「列举几条路径」的写法：这个洞就是这么来的 —— 旧实现列举了 7 类敏感
// 前缀，而 routes() 里有 60 多条路由，两者靠人眼对齐，于是 12 条写接口静默落进
// 「不敏感」分支：持发给下游客户端的 api_key 就能改配置、删模型隔离规则、清审计、
// 对全池发起上游写。
//
// 这里的断言对象是**注册表本身**（p.routeTable）：新增一条非 GET 路由而没在
// 「豁免清单」里显式登记，测试立刻红 —— 不需要有人记得回来加用例。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// c01WriteRoutes 审查报告点名的 12 条写路径（全部必须判敏感 + HTTP 403）。
var c01WriteRoutes = []string{
	"/panel/api/model_hidden",
	"/panel/api/model_effort",
	"/panel/api/model_rules",
	"/panel/api/model_rules/delete",
	"/panel/api/model_rules/apply",
	"/panel/api/request_logs/clear",
	"/panel/api/checkin_all",
	"/panel/api/travel_all",
	"/panel/api/activity_all",
	"/panel/api/keepalive_all",
	"/panel/api/balance_all",
	"/panel/api/usage/save",
}

// sampleRoutePath 把注册模式展开成一条具体请求路径：{uid} → u1，{$} → 空尾。
func sampleRoutePath(pattern string) string {
	segs := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	for i, seg := range segs {
		switch {
		case seg == "{$}":
			segs[i] = ""
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			segs[i] = "u1"
		}
	}
	return "/" + strings.Join(segs, "/")
}

// TestEveryNonGetRouteIsSensitive 遍历注册表：每条非 GET 都必须判敏感，
// 唯一例外是 POST /panel/api/admin_key（由一次性 bootstrap_token 把守，见 W3-02）。
//
// 变异验证（C-01 必做）：把 panelSensitivePath 改回旧的「列举敏感 + 默认 false」，
// 本用例会在 POST /panel/api/model_hidden 上红 —— 这正是旧实现的漏项。
func TestEveryNonGetRouteIsSensitive(t *testing.T) {
	p := New(Config{Version: "test"})
	exemptNonGet := map[string]bool{
		"POST /panel/api/admin_key": true, // 自举入口：令牌在 adminkey.go 校验
	}
	seen := 0
	for _, rt := range p.routeTable {
		path := sampleRoutePath(rt.path)
		key := rt.method + " " + rt.path
		var want bool
		if rt.method == http.MethodGet {
			// GET 也只有显式列进只读豁免表的才放行，其余同样敏感。
			_, exempt := panelReadOnlyExempt["GET "+rt.path]
			want = !exempt
		} else {
			want = !exemptNonGet[key]
			seen++
		}
		if got := p.isSensitiveRequest(rt.method, path); got != want {
			t.Errorf("%s %s（模式 %s）：isSensitiveRequest=%v want %v",
				rt.method, path, rt.path, got, want)
		}
	}
	if seen < 40 {
		t.Fatalf("只遍历到 %d 条非 GET 路由：注册表没记全（routes() 里用了裸 mux.HandleFunc？）", seen)
	}
}

// TestC01TwelveWriteRoutesAreSensitive 把审查报告点名的 12 条钉死：
// 既要在判定层敏感，也要在 HTTP 层用客户端 key 得到 403。
func TestC01TwelveWriteRoutesAreSensitive(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New(""), APIKey: "client-key"})
	for _, path := range c01WriteRoutes {
		if !p.isSensitiveRequest(http.MethodPost, path) {
			t.Errorf("POST %s 判成了不敏感（C-01 原样复现）", path)
		}
		if !panelSensitivePath(http.MethodPost, path) {
			t.Errorf("panelSensitivePath(POST, %s) = false", path)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer client-key")
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s 用客户端 key：code=%d want 403", path, rec.Code)
		}
	}
}

// TestReadOnlyExemptOnlyRegisteredRoutes 反向守：只读豁免表里不能有悬空项。
//
// 两个方向都要守 —— 删了路由忘了删豁免，将来同路径被其它用途复用时就会「白名单放行」。
func TestReadOnlyExemptOnlyRegisteredRoutes(t *testing.T) {
	p := New(Config{Version: "test"})
	registered := map[string]bool{}
	for _, rt := range p.routeTable {
		registered[rt.method+" "+rt.path] = true
	}
	for key := range panelReadOnlyExempt {
		if !registered[key] {
			t.Errorf("只读豁免表里的 %s 没有对应路由（悬空条目）", key)
		}
	}
	// 豁免表只能放 GET：非 GET 一律敏感是这条修复的核心不变量。
	for key := range panelReadOnlyExempt {
		if !strings.HasPrefix(key, "GET ") {
			t.Errorf("只读豁免表混入非 GET：%s", key)
		}
	}
}

// TestUnregisteredPathIsSensitive 未登记路径（404 / ServeMux 归一 / 将来新增但忘了
// 登记）必须按敏感处理 —— 失效方向只能是不放行。
func TestUnregisteredPathIsSensitive(t *testing.T) {
	p := New(Config{Version: "test"})
	for _, c := range []struct{ method, path string }{
		{"POST", "/panel/api/not_registered"},
		{"GET", "/panel/api/not_registered"},
		{"POST", "/panel/api/accounts"},          // 只注册了账号子路径，没有裸 /accounts POST
		{"POST", "/panel/api/overview"},          // 只注册了 GET
		{"GET", "/panel/api/accounts/u1/remove"}, // 只注册了 POST
	} {
		if !p.isSensitiveRequest(c.method, c.path) {
			t.Errorf("%s %s 未登记却判成不敏感", c.method, c.path)
		}
	}
}

// TestSensitiveRoutesDenyClientKeyEndToEnd 端到端：没配 admin_api_key 时，客户端 key
// 只能读只读观测接口，任何写接口 403；只读页仍 200（不能把面板一刀切锁死）。
func TestSensitiveRoutesDenyClientKeyEndToEnd(t *testing.T) {
	p := New(Config{Version: "test", Pool: pool.New(""), APIKey: "client-key"})
	for _, path := range []string{
		"/panel/api/overview",
		"/panel/api/error_summary",
		"/panel/api/request_metrics",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer client-key")
		p.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("GET %s 是只读观测接口，客户端 key 不该被门禁拦下：code=%d", path, rec.Code)
		}
	}
	// 审计面不豁免：请求归档带 client_ip / UA，日志导出是整包审计。
	for _, path := range []string{
		"/panel/api/request_logs",
		"/panel/api/logs/export",
		"/panel/api/credit_history",
		"/panel/api/config",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer client-key")
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s：code=%d want 403（审计/配置面不豁免）", path, rec.Code)
		}
	}
}
