package panel

// 登录业务码分流（对齐 CLI 2.174.0 的 SignLicenseError）。
//
// 没有可登录的真实账号时，靠把端点指到 httptest 服务器把判据钉死：
// 终态码必须立刻报错并透出上游 msg，不能像以前那样空转到 15 分钟 TTL。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/loginsite"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

const loginTestState = "teststate-1234567890"

// loginTestPanel 造一个只够跑 loginStart/loginPoll 的 Panel，端点指向 srv。
func loginTestPanel(t *testing.T, srv *httptest.Server) *Panel {
	t.Helper()
	old := loginEndpointsFn
	loginEndpointsFn = func(string) (string, string, string, string) {
		return srv.URL + "/state", srv.URL + "/token?state=", srv.URL + "/account?state=", "https://www.codebuddy.cn"
	}
	t.Cleanup(func() { loginEndpointsFn = old })

	pn := &Panel{cfg: Config{Pool: pool.New("")}, logins: map[string]*loginSession{}}
	pn.logins[loginTestState] = &loginSession{created: time.Now(), realm: "cn"}
	return pn
}

func pollOnce(t *testing.T, pn *Panel) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	pn.loginPoll(rec, httptest.NewRequest(http.MethodGet, "/panel/api/login/poll?state="+loginTestState, nil))
	return rec
}

// 终态许可错误：立刻 502 + 上游 msg，并清掉会话（前端据此停止轮询、提示重试）。
func TestLoginPollFatalCodeReportsImmediately(t *testing.T) {
	for _, code := range []int{loginsite.CodeLicenseSeatLimit, loginsite.CodeLicenseExpired, loginsite.CodeTrialExpired} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":` + itoa(code) + `,"msg":"许可席位已满，请先释放一个席位"}`))
		}))
		pn := loginTestPanel(t, srv)
		rec := pollOnce(t, pn)
		srv.Close()

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("code=%d status=%d want 502 body=%s", code, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "许可席位已满") {
			t.Fatalf("code=%d 上游 msg 没透出: %s", code, rec.Body.String())
		}
		if _, still := pn.logins[loginTestState]; still {
			t.Fatalf("code=%d 终态后会话应被清掉（否则前端会拿着同一个 state 转到 TTL）", code)
		}
	}
}

// pending 码：继续轮询，{done:false}，会话保留。
func TestLoginPollPendingCodeKeepsPolling(t *testing.T) {
	for _, code := range []int{loginsite.CodeRetryFetchToken, loginsite.CodeRetryFetchAccount} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":` + itoa(code) + `,"msg":"login ing"}`))
		}))
		pn := loginTestPanel(t, srv)
		rec := pollOnce(t, pn)
		srv.Close()

		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d status=%d want 200", code, rec.Code)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got["done"] != false {
			t.Fatalf("code=%d done=%v want false", code, got["done"])
		}
		if _, still := pn.logins[loginTestState]; !still {
			t.Fatalf("code=%d pending 时不该清会话", code)
		}
	}
}

// 码表外的码按 pending 处理：宁可多转，别把上游新增的中间态误报成致命错误。
func TestLoginPollUnknownCodeTreatedAsPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":9999,"msg":"something new"}`))
	}))
	pn := loginTestPanel(t, srv)
	rec := pollOnce(t, pn)
	srv.Close()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200（未知码按 pending）body=%s", rec.Code, rec.Body.String())
	}
}

// 发起登录时授权链接必须补 version（官方登录页按它做灰度与埋点）。
func TestLoginStartDecoratesAuthURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"abc12345","authUrl":"https://login.test/authorize?state=abc12345"}}`))
	}))
	pn := loginTestPanel(t, srv)
	rec := httptest.NewRecorder()
	pn.loginStart(rec, httptest.NewRequest(http.MethodPost, "/panel/api/login/start", strings.NewReader(`{"realm":"cn"}`)))
	srv.Close()

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, _ := got["url"].(string)
	if !strings.Contains(u, "version="+loginsite.ClientVersion) {
		t.Fatalf("授权链接缺 version: %s", u)
	}
}

// itoa 避免为几个用例引入 strconv（也让用例体读起来更直白）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
