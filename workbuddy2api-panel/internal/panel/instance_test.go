// instance_test.go GET /panel/api/instance 的契约钉（W6-H-02 的 Go 侧）。
//
// 这个端点是**唯一免鉴权**的 API，所以每条约束都要有用例：只回环、只回 32 hex、
// 多次调用同一个值、文件缺失时生成后可读回、**绝不回显请求里的任何东西**。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var instanceHex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// instancePanel 造一个指定了实例文件路径的面板（不落 config.json）。
func instancePanel(t *testing.T, path string) *Panel {
	t.Helper()
	return New(Config{Version: "test", InstanceIDPath: path})
}

// instanceGET 直接调 handler（带来源地址），返回 recorder。
func instanceGET(t *testing.T, p *Panel, remote, target string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.RemoteAddr = remote
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	return rec
}

func decodeInstanceID(t *testing.T, body []byte) string {
	t.Helper()
	var got struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, body)
	}
	return got.InstanceID
}

// TestInstanceEndpointLoopbackReturnsHex32：回环 200，返回 32 位小写 hex。
func TestInstanceEndpointLoopbackReturnsHex32(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", instanceIDFile)
	p := instancePanel(t, path)
	rec := instanceGET(t, p, "127.0.0.1:51234", "/panel/api/instance")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	id := decodeInstanceID(t, rec.Body.Bytes())
	if !instanceHex32.MatchString(id) {
		t.Fatalf("instance_id=%q 不是 32 位小写 hex", id)
	}
	// 文件也必须真的落盘（壳读的是文件，不是 HTTP 响应）：0600，内容就是它。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("实例文件没生成: %v", err)
	}
	if string(raw) != id {
		t.Fatalf("文件内容=%q 与响应 %q 不一致", raw, id)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("权限=%o want 0600", perm)
	}
}

// TestInstanceEndpointRefusesNonLoopback：非回环一律 403（这个端点没有鉴权，
// 能远程调就等于给公网一个指纹面）。
func TestInstanceEndpointRefusesNonLoopback(t *testing.T) {
	path := filepath.Join(t.TempDir(), instanceIDFile)
	p := instancePanel(t, path)
	rec := instanceGET(t, p, "192.0.2.9:4433", "/panel/api/instance")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403（body=%s）", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("被拒绝的请求不该生成文件: %v", err)
	}
}

// TestInstanceEndpointStableAcrossCalls：同一个进程两次调用必须是同一个 id，
// 且第二次不再重写文件（注入校验靠「文件 == 响应」，漂移一次就永远失败）。
func TestInstanceEndpointStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, instanceIDFile)
	p := instancePanel(t, path)
	first := decodeInstanceID(t, instanceGET(t, p, "127.0.0.1:1", "/panel/api/instance").Body.Bytes())
	st1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	second := decodeInstanceID(t, instanceGET(t, p, "[::1]:2", "/panel/api/instance").Body.Bytes())
	if first != second {
		t.Fatalf("两次调用 id 不同：%q vs %q", first, second)
	}
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st1.ModTime().Equal(st2.ModTime()) || st1.Size() != st2.Size() {
		t.Fatal("第二次调用重写了实例文件")
	}
}

// TestInstanceEndpointReadsExistingFile：壳先写好文件、Go 后启动（Android 的真实顺序）——
// 必须读同一个值，而不是另生成一个。
func TestInstanceEndpointReadsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", instanceIDFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const want = "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	p := instancePanel(t, path)
	got := decodeInstanceID(t, instanceGET(t, p, "127.0.0.1:1", "/panel/api/instance").Body.Bytes())
	if got != want {
		t.Fatalf("instance_id=%q want %q（没读壳写下的文件）", got, want)
	}
}

// TestInstanceEndpointRegeneratesCorruptFile：半截串/非法内容当作「没有」，重新生成
// 一个合法值并覆盖 —— 否则校验永远不匹配，且用户完全看不出原因。
func TestInstanceEndpointRegeneratesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), instanceIDFile)
	if err := os.WriteFile(path, []byte("deadbeef"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := instancePanel(t, path)
	got := decodeInstanceID(t, instanceGET(t, p, "127.0.0.1:1", "/panel/api/instance").Body.Bytes())
	if !instanceHex32.MatchString(got) {
		t.Fatalf("损坏文件应重新生成 32 hex，得到 %q", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != got {
		t.Fatalf("重新生成后文件内容应为 %q，得到 %q（err=%v）", got, raw, err)
	}
}

// TestInstanceEndpointNeverEchoesRequest：绝不回显请求里的任何参数 ——
// 否则占位进程只要原样反射一个 ?instance_id= 就能通过壳的校验。
func TestInstanceEndpointNeverEchoesRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), instanceIDFile)
	p := instancePanel(t, path)
	const probe = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rec := instanceGET(t, p, "127.0.0.1:1", "/panel/api/instance?instance_id="+probe+"&x=%3Cscript%3E")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if len(body) > 128 {
		t.Fatalf("响应体过大（可能回显了请求）: %s", body)
	}
	if got := decodeInstanceID(t, rec.Body.Bytes()); got == probe {
		t.Fatal("回显了请求里的 instance_id —— 占位进程可以反射通过校验")
	}
	if containsAny(body, "<script", "instance_id=", "x=") {
		t.Fatalf("响应里出现了请求参数: %s", body)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

// TestPanelPublicPathsAreExplicit：免鉴权路径必须是一张写死的完整表。
// 这条守的是「将来有人图省事加个前缀匹配」—— 那等于把整个 /panel/api 放开。
func TestPanelPublicPathsAreExplicit(t *testing.T) {
	for _, p := range []string{"/panel/", "/panel/theme.js", "/panel/app.js", "/panel/api/instance"} {
		if !panelPublicPath(p) {
			t.Errorf("%s 应免鉴权", p)
		}
	}
	for _, p := range []string{
		"/panel/api", "/panel/api/", "/panel/api/overview", "/panel/api/export/accounts",
		"/panel/api/instance/", "/panel/api/instancex", "/panel/app.js/../api/config",
	} {
		if panelPublicPath(p) {
			t.Errorf("%s 不该免鉴权（表必须写死完整路径，不能前缀匹配）", p)
		}
	}
}
