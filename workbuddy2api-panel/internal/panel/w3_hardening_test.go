// w3_hardening_test.go W3 分片的安全加固回归钉（W3-02/03/05/06/11/14/15/20）。
//
// 这一组用例的共同点：**修的都是「失效方向不安全」的默认值**。默认放行、默认不脱敏、
// 默认无上限 —— 单看每条都不起眼，合起来就是「面板在自己的威胁模型里是可信的，
// 而真实部署（Android 回环跨 App 共享、明文 HTTP、api_key 可能为空）不是」。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// ---------------------------------------------------------------------------
// W3-02 自举令牌
// ---------------------------------------------------------------------------

// TestAdminKeyBootstrapRequiresToken：没有令牌/令牌错误一律 403，且不落盘。
// 这条替代了「回环即放行」的旧判据 —— Android 上回环是跨 App 共享的。
func TestAdminKeyBootstrapRequiresToken(t *testing.T) {
	cases := []struct{ name, body string }{
		{"缺令牌", `{"admin_api_key":"0123456789abcdef0123"}`},
		{"空令牌", `{"admin_api_key":"0123456789abcdef0123","bootstrap_token":""}`},
		{"错令牌", `{"admin_api_key":"0123456789abcdef0123","bootstrap_token":"deadbeefdeadbeefdeadbeefdeadbeef"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var saved []byte
			p := newBootstrapPanel(&saved)
			// 这里必须**原样**发 body：postAdminKey 会自动补令牌，正好会把这个用例
			// 想测的「缺令牌」补掉。
			r := httptest.NewRequest(http.MethodPost, "/panel/api/admin_key", strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer client-key-123456")
			r.RemoteAddr = "127.0.0.1:51234"
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d want 403（body=%s）", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "bootstrap_token_required") {
				t.Fatalf("错误码应可辨识: %s", rec.Body.String())
			}
			if len(saved) != 0 {
				t.Fatal("令牌不对却落盘了")
			}
		})
	}
}

// TestAdminKeyBootstrapTokenIsSingleUse：令牌用一次即作废（内存抹掉 + 文件删除）。
// 不这样做的话，本地 App 只要抓到过一次令牌就能反复把管理员密钥改成自己的值。
func TestAdminKeyBootstrapTokenIsSingleUse(t *testing.T) {
	var saved []byte
	p := newBootstrapPanel(&saved)
	tok := p.BootstrapToken()
	if tok == "" {
		t.Fatal("进程启动后应已生成自举令牌")
	}
	body := `{"admin_api_key":"0123456789abcdef0123","bootstrap_token":"` + tok + `"}`
	rec := postAdminKey(t, p, "127.0.0.1:51234", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次自举 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if p.BootstrapToken() != "" {
		t.Fatal("成功后令牌必须置空")
	}
	// 同一个令牌再来一次：即使 SaveConfig 没真的改掉 cfg（测试桩），也必须被拒。
	rec = postAdminKey(t, p, "127.0.0.1:51234", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("重放 status=%d want 403", rec.Code)
	}
}

// TestAdminKeyBootstrapTokenFileMode：令牌落盘到 config 同目录，0600，内容就是令牌。
// 权限是这条设计的核心 —— 令牌文件可读 = 本机任意 App 都能自举。
func TestAdminKeyBootstrapTokenFileMode(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{
		Version:    "test",
		APIKey:     "client-key-123456",
		ConfigPath: filepath.Join(dir, "config.json"),
		SaveConfig: func([]byte) ([]string, error) { return nil, nil },
	})
	path := filepath.Join(dir, bootstrapTokenFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("令牌文件没生成: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("权限 = %o want 0600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != p.BootstrapToken() {
		t.Fatalf("文件内容与内存令牌不一致")
	}
	// 用掉之后文件要删掉：留着就等于「令牌永不过期」。
	p.burnBootstrapToken()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("令牌用掉后文件应删除，stat err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// W3-03 跨站写 / Content-Type
// ---------------------------------------------------------------------------

// postPanel 发一个 POST，返回状态码。用未登记路径收尾（404），便于区分
// 「被闸门拦下」（403/415）与「过了闸门但路由不存在」（404）。
func postPanel(t *testing.T, h http.Header, path string) int {
	t.Helper()
	p := New(Config{Version: "test"}) // 无 key：隔离出闸门本身的行为
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	for k, v := range h {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec.Code
}

// TestNonGetRejectsCrossSiteOrigin：跨站 Origin 一律 403（含端口不同也算跨源）。
func TestNonGetRejectsCrossSiteOrigin(t *testing.T) {
	path := "/panel/api/not_registered"
	for _, origin := range []string{
		"http://evil.example",
		"http://127.0.0.1:9999", // 同主机不同端口 = 不同源（本地其它 App 的页面）
		"null",
	} {
		code := postPanel(t, http.Header{"Origin": {origin}, "Content-Type": {"application/json"}}, path)
		if code != http.StatusForbidden {
			t.Errorf("Origin=%s code=%d want 403", origin, code)
		}
	}
	// 同源放行（Host 由 httptest 设为 example.com）。
	code := postPanel(t, http.Header{"Origin": {"http://example.com"}, "Content-Type": {"application/json"}}, path)
	if code != http.StatusNotFound {
		t.Errorf("同源请求 code=%d want 404（过了闸门）", code)
	}
}

// TestNonGetUsesRefererAndSecFetchSite：没有 Origin 时依次看 Referer / Sec-Fetch-Site；
// 三者都没有（curl / 脚本 / 移动端）必须放行 —— 不能为了防 CSRF 把 CLI 用户挡死。
func TestNonGetUsesRefererAndSecFetchSite(t *testing.T) {
	path := "/panel/api/not_registered"
	cases := []struct {
		name string
		h    http.Header
		want int
	}{
		{"Referer 跨站", http.Header{"Referer": {"http://evil.example/x"}}, http.StatusForbidden},
		{"Referer 同源", http.Header{"Referer": {"http://example.com/panel/"}}, http.StatusNotFound},
		{"Sec-Fetch-Site 跨站", http.Header{"Sec-Fetch-Site": {"cross-site"}}, http.StatusForbidden},
		{"Sec-Fetch-Site 同源", http.Header{"Sec-Fetch-Site": {"same-origin"}}, http.StatusNotFound},
		{"无任何浏览器头（curl）", http.Header{}, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.h.Set("Content-Type", "application/json")
			if got := postPanel(t, c.h, path); got != c.want {
				t.Fatalf("code=%d want %d", got, c.want)
			}
		})
	}
}

// postPanelBody 同 postPanel，但由调用方决定 body（用来区分「空 body」与「有字段」）。
func postPanelBody(t *testing.T, ct, body string) int {
	t.Helper()
	p := New(Config{Version: "test"})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/not_registered", strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec.Code
}

// TestNonGetAllowsEmptyBodyFormEncoding：Android 的 HttpURLConnection 会给「没有 body
// 的 POST」自动补 application/x-www-form-urlencoded（OkHttp HttpEngine 的
// hasRequestBody() 对 POST 恒为真），原生控制台的签到 / 刷额度 / 一键完成全是空 body
// 的 POST —— 线上症状是「签到失败：HTTP 415：unsupported_media_type」。
//
// 边界必须两边都钉住：**空 body** 放行、**带字段的表单体**照旧 415。
func TestNonGetAllowsEmptyBodyFormEncoding(t *testing.T) {
	const form = "application/x-www-form-urlencoded"
	if code := postPanelBody(t, form, ""); code != http.StatusNotFound {
		t.Fatalf("空 body + 表单编码 code=%d want 404（放行，等同无 body 的 POST）", code)
	}
	for _, body := range []string{"{}", "a=1", "file=@x"} {
		if code := postPanelBody(t, form, body); code != http.StatusUnsupportedMediaType {
			t.Errorf("表单编码 body=%q code=%d want 415", body, code)
		}
	}
	// text/plain 不放宽：fetch(mode:'no-cors') 用它发简单请求，是最典型的跨站形态。
	if code := postPanelBody(t, "text/plain", ""); code != http.StatusUnsupportedMediaType {
		t.Errorf("空 body + text/plain code=%d want 415（不放宽）", code)
	}
	// 分块（ContentLength=-1）也算「有 body」，不能被空 body 例外捞出来。
	req := httptest.NewRequest(http.MethodPost, "/panel/api/not_registered", strings.NewReader(""))
	req.ContentLength = -1
	req.Header.Set("Content-Type", form)
	rec := httptest.NewRecorder()
	New(Config{Version: "test"}).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("分块 + 表单编码 code=%d want 415", rec.Code)
	}
}

// TestNonGetRejectsSimpleContentTypes：text/plain / 表单编码是 CORS「简单请求」，
// 浏览器不预检就能跨站发出 —— 明确 415。multipart 留给导入上传。
func TestNonGetRejectsSimpleContentTypes(t *testing.T) {
	path := "/panel/api/not_registered"
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "text/plain;charset=UTF-8"} {
		if code := postPanel(t, http.Header{"Content-Type": {ct}}, path); code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type=%s code=%d want 415", ct, code)
		}
	}
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "multipart/form-data; boundary=x", ""} {
		if code := postPanel(t, http.Header{"Content-Type": {ct}}, path); code != http.StatusNotFound {
			t.Errorf("Content-Type=%q code=%d want 404（放行）", ct, code)
		}
	}
}

// ---------------------------------------------------------------------------
// W3-05 导入条数上限覆盖字典/信封形态
// ---------------------------------------------------------------------------

func buildDictImport(n int) []byte {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"u%05d":{"access_token":"a","refresh_token":"r"}`, i)
	}
	b.WriteString("}")
	return []byte(b.String())
}

func buildEnvelopeImport(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"accounts":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"uid":"u%05d","access_token":"a","refresh_token":"r"}`, i)
	}
	b.WriteString("]}")
	return []byte(b.String())
}

// TestImportEntryLimitCoversAllShapes：数组 / 信封 / 字典三种形态都必须卡在
// importMaxEntries 上（旧实现只对根数组生效，字典形态直接 return）。
func TestImportEntryLimitCoversAllShapes(t *testing.T) {
	over := importMaxEntries + 1
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{"字典形态", buildDictImport(over)},
		{"信封形态", buildEnvelopeImport(over)},
	} {
		t.Run(c.name, func(t *testing.T) {
			// ① 字节级预检自己就要挡住（不依赖解码后再判）—— 否则 32MB 的
			//    {"uid":{...}} 会先被 json.Unmarshal 展开成几十万个 RawMessage。
			if err := checkImportShape(c.raw); err == nil {
				t.Fatalf("%s %d 条：checkImportShape 没拦住（解码前就该拒）", c.name, over)
			}
			// ② 解析入口同样拒绝（两道闸，任一失效都不该放行）。
			if _, err := parseImportPayload(c.raw); err == nil {
				t.Fatalf("%s %d 条：应被上限拒绝", c.name, over)
			}
		})
	}
	// 边界内的字典形态正常解析。
	small := buildDictImport(3)
	list, err := parseImportPayload(small)
	if err != nil {
		t.Fatalf("3 条字典形态应可解析: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("解析出 %d 条 want 3", len(list))
	}
	if list[0].UID != "u00000" {
		t.Fatalf("字典键应兜底为 uid，得到 %q", list[0].UID)
	}
}

// ---------------------------------------------------------------------------
// W3-06 / W3-11 日志清洗
// ---------------------------------------------------------------------------

// TestSanitizeLogTextStripsNewlinesAndCaps：换行/回车/控制字符必须去掉（否则可以
// 用 uid 或禁用原因伪造整行日志），单行必须有上限。
func TestSanitizeLogTextStripsNewlinesAndCaps(t *testing.T) {
	got := sanitizeLogText("uid=x\nERR: 池已清空\r\nnext\x01")
	if strings.ContainsAny(got, "\r\n\x01") {
		t.Fatalf("仍含换行/控制字符: %q", got)
	}
	if !strings.Contains(got, "uid=x") || !strings.Contains(got, "ERR: 池已清空") {
		t.Fatalf("内容被误删: %q", got)
	}
	long := sanitizeLogText(strings.Repeat("A", logTextMaxBytes*2))
	if len(long) > logTextMaxBytes+len("…(truncated)") {
		t.Fatalf("单行未截断: len=%d", len(long))
	}
	if !strings.HasSuffix(long, "…(truncated)") {
		t.Fatalf("截断应留标记: %q", long[len(long)-20:])
	}
}

// TestRingStoresSanitizedText：内存环与落盘共用同一个清洗函数（W3-11）。
// 之前环里是原文：「面板里能看到 Authorization」本身就已经是泄漏。
func TestRingStoresSanitizedText(t *testing.T) {
	r := NewRing(8)
	if _, err := r.Write([]byte("call upstream Authorization: Bearer abcdefgh12345678\r\n")); err != nil {
		t.Fatal(err)
	}
	es := r.Snapshot()
	if len(es) != 1 {
		t.Fatalf("entries=%d", len(es))
	}
	if strings.Contains(es[0].Text, "abcdefgh12345678") {
		t.Fatalf("内存环未脱敏: %q", es[0].Text)
	}
	if strings.Contains(es[0].Text, "\r") {
		t.Fatalf("内存环未剥回车: %q", es[0].Text)
	}
}

// ---------------------------------------------------------------------------
// W3-14 导出闸门
// ---------------------------------------------------------------------------

// TestLogsExportConcurrencyGate：同一时刻只允许一个导出，第二个立刻 429。
func TestLogsExportConcurrencyGate(t *testing.T) {
	p, _ := newExportPanel(t)
	p.logExportSem <- struct{}{} // 模拟「已有一个在打包」
	defer func() { <-p.logExportSem }()
	rec := httptest.NewRecorder()
	p.logsExportHandler(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs/export?days=1", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "export_in_progress") {
		t.Fatalf("错误码应可辨识: %s", rec.Body.String())
	}
}

// TestCappedWriterStopsAtLimit：体积闸门在**写的过程中**就中断，而不是写完再看大小。
func TestCappedWriterStopsAtLimit(t *testing.T) {
	var sb strings.Builder
	w := &cappedWriter{w: &sb, max: 10}
	if _, err := w.Write(make([]byte, 8)); err != nil {
		t.Fatalf("未超限不该报错: %v", err)
	}
	if _, err := w.Write(make([]byte, 8)); !errors.Is(err, errExportTooLarge) {
		t.Fatalf("超限应返回哨兵错误，得到 %v", err)
	}
	if sb.Len() != 8 {
		t.Fatalf("超限的那次不该写进去: len=%d", sb.Len())
	}
}

// ---------------------------------------------------------------------------
// W3-15 日志分页
// ---------------------------------------------------------------------------

func TestLogsHandlerLimit(t *testing.T) {
	p := New(Config{Version: "test"})
	for i := 0; i < 5; i++ {
		p.logs.Write([]byte(fmt.Sprintf("line-%d", i)))
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs?limit=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Entries []LogEntry `json:"entries"`
		Total   int        `json:"total"`
		Limit   int        `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Total != 5 || got.Limit != 2 {
		t.Fatalf("entries=%d total=%d limit=%d want 2/5/2", len(got.Entries), got.Total, got.Limit)
	}
	// 取的是**最近**两条（环内时间升序，截尾）。
	if got.Entries[1].Text != "line-4" {
		t.Fatalf("最后一条 = %q want line-4", got.Entries[1].Text)
	}
	// 非法 limit 明确 400，不静默回落。
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs?limit=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 limit code=%d want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// W5-04 空 key 语义与 server 对齐
// ---------------------------------------------------------------------------

// TestPanelEmptyKeyHonorsAllowEmptyKey：面板不再自己判「key == 空 → 放行」，
// 而是读 httpauth 的统一开关。关掉后：
//   - 回环请求照常（Android 内嵌形态全部是回环，不能一刀切把面板打死）；
//   - 非回环 401（反代/局域网 = 公网入口，不能匿名）。
func TestPanelEmptyKeyHonorsAllowEmptyKey(t *testing.T) {
	httpauth.SetAllowEmptyKey(false)
	t.Cleanup(func() { httpauth.SetAllowEmptyKey(true) })

	p := New(Config{Version: "test", Pool: pool.New("")})
	// 回环：200。
	r := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	r.RemoteAddr = "127.0.0.1:51234"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("回环 code=%d want 200（body=%s）", rec.Code, rec.Body.String())
	}
	// 非回环：401。
	r = httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	r.RemoteAddr = "192.0.2.9:4433"
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("非回环 code=%d want 401", rec.Code)
	}
	// 伪造 X-Forwarded-For 不该放行（判据只看 RemoteAddr）。
	r = httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	r.RemoteAddr = "192.0.2.9:4433"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("XFF 伪造放行了：code=%d", rec.Code)
	}
}

// TestOverviewExposesSecurityPosture：overview 必须透出前端横幅要的两个字段。
//   - empty_key_only_loopback：与 httpauth 开关同口径（不是前端自己猜）；
//   - at_rest_key：auth.AtRestKeySource() 的元数据（不含密钥材料）。
func TestOverviewExposesSecurityPosture(t *testing.T) {
	httpauth.SetAllowEmptyKey(false)
	t.Cleanup(func() { httpauth.SetAllowEmptyKey(true) })

	p := New(Config{Version: "test", Pool: pool.New("")})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.RemoteAddr = "127.0.0.1:51234" // 开关关掉后只有回环能进
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if v, ok := d["empty_key_only_loopback"].(bool); !ok || !v {
		t.Fatalf("empty_key_only_loopback=%v want true", d["empty_key_only_loopback"])
	}
	src, ok := d["at_rest_key"].(map[string]any)
	if !ok {
		t.Fatalf("at_rest_key 字段缺失或类型不对: %#v", d["at_rest_key"])
	}
	want := auth.AtRestKeySource()
	if src["source"] != want.Source || src["builtin"] != want.Builtin {
		t.Fatalf("at_rest_key=%v 与 auth.AtRestKeySource()=%+v 不一致", src, want)
	}

	// 开关打开时字段必须翻回 false（默认部署形态）。
	httpauth.SetAllowEmptyKey(true)
	req = httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.RemoteAddr = "127.0.0.1:51234"
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if v, _ := d["empty_key_only_loopback"].(bool); v {
		t.Fatal("AllowEmptyKey=true 时仍报 true")
	}
}

// ---------------------------------------------------------------------------
// W3-20 不回显服务器绝对路径
// ---------------------------------------------------------------------------

func TestReloadStatusDoesNotLeakPath(t *testing.T) {
	secret := "/home/operator/private-auths"
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: secret})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/accounts/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "auth_dir") || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("响应仍回显服务器路径: %s", rec.Body.String())
	}
}
