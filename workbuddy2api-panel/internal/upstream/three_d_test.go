package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// newThreeDClient 造一个只会走 fake transport 的 Client，并挂上指定凭证表。
func newThreeDClient(t *testing.T, tokens map[string]string, rt http.RoundTripper) *Client {
	t.Helper()
	c := &Client{ChatBaseCN: "https://copilot.tencent.com", HTTP: &http.Client{Transport: rt}}
	if tokens != nil {
		c.ThreeDCredential = NewStaticThreeDCredentials(tokens)
	}
	return c
}

func TestThreeDHeadersAndCall(t *testing.T) {
	now := time.Unix(1794551118, 0).UTC()
	h := threeDHeaders("copilot.tencent.com", threeDSubmitAction, []byte(`{"Model":"3.1"}`), "ck_t_abc", now)
	// 凭证前缀是协议既定的 hy-3d.，后面跟的是临时云凭证——不是账号 OAuth token。
	if !strings.Contains(h["Authorization"], "Credential=hy-3d.ck_t_abc/2026-11-13/ai3d/tc3_request") {
		t.Fatalf("bad TC3 authorization: %s", h["Authorization"])
	}
	var gotAction, gotBody string
	c := newThreeDClient(t, map[string]string{"u1": "ck_t_abc"}, rtFunc(func(r *http.Request) (*http.Response, error) {
		gotAction = r.Header.Get("X-TC-Action")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"Response":{"JobId":"job1","RequestId":"rid1"}}`))}, nil
	}))
	a := &auth.Auth{UID: "u1", AccessToken: "oauth-token", ExpiresAt: 9999999999}
	raw, err := c.ThreeDGenerations(context.Background(), a, map[string]string{"Model": "3.1"})
	if err != nil || ParseThreeDState(raw, "").JobID != "job1" {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
	if gotAction != threeDSubmitAction || gotBody != `{"Model":"3.1"}` {
		t.Fatalf("action=%s body=%s", gotAction, gotBody)
	}
}

// 没有临时凭证时必须在发请求之前就失败：不拿 OAuth token 顶替，也不打上游。
func TestThreeDNoCredentialDoesNotCallUpstream(t *testing.T) {
	calls := 0
	c := newThreeDClient(t, nil, rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	}))
	_, err := c.ThreeDGenerations(context.Background(), &auth.Auth{UID: "u1", AccessToken: "oauth-token", ExpiresAt: 9999999999}, map[string]string{"Model": "3.1"})
	if !errors.Is(err, ErrThreeDNoCredential) {
		t.Fatalf("err=%v want ErrThreeDNoCredential", err)
	}
	if calls != 0 {
		t.Fatalf("没有凭证时不该打上游，实际 %d 次", calls)
	}
}

// 凭证表里没有该账号也没有 "*" 兜底：同一结论（不可用，不发请求）。
func TestThreeDCredentialMissForAccount(t *testing.T) {
	calls := 0
	c := newThreeDClient(t, map[string]string{"other": "ck_t_x"}, rtFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	}))
	_, err := c.ThreeDTask(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "job")
	if !errors.Is(err, ErrThreeDNoCredential) {
		t.Fatalf("err=%v want ErrThreeDNoCredential", err)
	}
	if calls != 0 {
		t.Fatalf("不该打上游，实际 %d 次", calls)
	}
}

// "*" 兜底对所有账号生效；按 UID 精确命中的优先。
func TestStaticThreeDCredentialsFallback(t *testing.T) {
	s := NewStaticThreeDCredentials(map[string]string{"*": "ck_t_star", "u2": "ck_t_two"})
	if tok, err := s.TempToken(context.Background(), &auth.Auth{UID: "u1"}); err != nil || tok != "ck_t_star" {
		t.Fatalf("u1 token=%q err=%v", tok, err)
	}
	if tok, err := s.TempToken(context.Background(), &auth.Auth{UID: "u2"}); err != nil || tok != "ck_t_two" {
		t.Fatalf("u2 token=%q err=%v", tok, err)
	}
	// 空白值视为未配置。
	s.Replace(map[string]string{"u1": "   "})
	if tok, err := s.TempToken(context.Background(), &auth.Auth{UID: "u1"}); !errors.Is(err, ErrThreeDNoCredential) || tok != "" {
		t.Fatalf("空白凭证应视为缺失：tok=%q err=%v", tok, err)
	}
}

func TestParseThreeDStateResultFiles(t *testing.T) {
	raw := []byte(`{"Status":"DONE","ResultCreditConsumed":20,"ResultFile3Ds":[{"Type":"OBJ","Url":"obj.zip"},{"Type":"GLB","Url":"model.glb"}],"RequestId":"rid"}`)
	st := ParseThreeDState(raw, "job")
	if !st.Done() || !st.Succeeded() || st.ResultURL != "model.glb" || st.Credit != 20 {
		t.Fatalf("state=%+v", st)
	}
}

func TestThreeDBusinessError(t *testing.T) {
	c := newThreeDClient(t, map[string]string{"*": "ck_t_bad"}, rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"Response":{"Error":{"Code":"AuthFailure","Message":"bad token"}}}`))}, nil
	}))
	_, err := c.ThreeDTask(context.Background(), &auth.Auth{UID: "u1", AccessToken: "bad", ExpiresAt: 9999999999}, "job")
	if err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("err=%v", err)
	}
	// 错误串里不能出现凭证本身。
	if strings.Contains(err.Error(), "ck_t_bad") {
		t.Fatalf("错误信息泄露了凭证：%v", err)
	}
}

// ---- 自动签发（FetchClientTempKey + AutoThreeDCredentials）----

// newAutoKeyServer 起一个假 /v2/api-keys：记录请求并按给定响应回。
func newAutoKeyServer(t *testing.T, resp func(r *http.Request) string, got *map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if got != nil {
			(*got)["path"] = r.URL.Path
			(*got)["auth"] = r.Header.Get("Authorization")
			(*got)["ent"] = r.Header.Get("X-Enterprise-Id")
			(*got)["body"] = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp(r)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchClientTempKey(t *testing.T) {
	got := map[string]string{}
	srv := newAutoKeyServer(t, func(*http.Request) string {
		return `{"code":0,"data":{"key":"ck_t_abc.def","expires_at":"2099-01-02T03:04:05Z"}}`
	}, &got)
	c := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client()}
	a := &auth.Auth{UID: "u1", AccessToken: "oauth-token", ExpiresAt: 9999999999}
	key, exp, err := c.FetchClientTempKey(context.Background(), a)
	if err != nil || !strings.HasPrefix(key, "ck_t_") {
		t.Fatalf("key=%q err=%v", key, err)
	}
	if exp.IsZero() {
		t.Fatal("RFC3339 expires_at 应被解析")
	}
	if got["path"] != threeDKeyPath {
		t.Fatalf("path=%s", got["path"])
	}
	if got["auth"] != "Bearer oauth-token" {
		t.Fatalf("auth=%s", got["auth"])
	}
	if got["ent"] != "" {
		t.Fatalf("个人号不该带企业头：%s", got["ent"])
	}
	var body map[string]any
	if json.Unmarshal([]byte(got["body"]), &body) != nil ||
		body["name"] != threeDKeyName || body["temporary"] != true ||
		body["expire_in_seconds"] != float64(threeDKeyTTLSeconds) {
		t.Fatalf("body=%s", got["body"])
	}

	// 企业号注入 X-Enterprise-Id。
	got2 := map[string]string{}
	srv2 := newAutoKeyServer(t, func(*http.Request) string {
		return `{"code":0,"data":{"key":"ck_t_e","expires_at":4102444800}}`
	}, &got2)
	c2 := &Client{ChatBaseCN: srv2.URL, HTTP: srv2.Client()}
	key2, exp2, err := c2.FetchClientTempKey(context.Background(), &auth.Auth{UID: "u2", AccessToken: "at2", EnterpriseID: "ent-1", ExpiresAt: 9999999999})
	if err != nil || key2 != "ck_t_e" {
		t.Fatalf("key=%q err=%v", key2, err)
	}
	if got2["ent"] != "ent-1" {
		t.Fatalf("企业头缺失：%s", got2["ent"])
	}
	if exp2.IsZero() {
		t.Fatal("unix 秒 expires_at 应被解析")
	}
}

func TestFetchClientTempKeyFailures(t *testing.T) {
	srv := newAutoKeyServer(t, func(*http.Request) string {
		return `{"code":121000,"msg":"denied"}`
	}, nil)
	c := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client()}
	_, _, err := c.FetchClientTempKey(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	if !errors.Is(err, ErrThreeDNoCredential) {
		t.Fatalf("业务拒绝应归入 ErrThreeDNoCredential 族：%v", err)
	}

	// 静态 API Key 账号没有 OAuth 身份：短路，不打 HTTP。
	calls := 0
	srv2 := newAutoKeyServer(t, func(*http.Request) string {
		calls++
		return `{"code":0,"data":{"key":"ck_t_x"}}`
	}, nil)
	c2 := &Client{ChatBaseCN: srv2.URL, HTTP: srv2.Client()}
	_, _, err = c2.FetchClientTempKey(context.Background(), &auth.Auth{UID: "k1", AccessToken: "ck_abc.def", ExpiresAt: 9999999999})
	if !errors.Is(err, ErrThreeDNoCredential) || calls != 0 {
		t.Fatalf("API key 账号应短路：calls=%d err=%v", calls, err)
	}

	// 响应里不是 ck_t_ 前缀：拒绝。
	srv3 := newAutoKeyServer(t, func(*http.Request) string {
		return `{"code":0,"data":{"key":"not-temp"}}`
	}, nil)
	c3 := &Client{ChatBaseCN: srv3.URL, HTTP: srv3.Client()}
	_, _, err = c3.FetchClientTempKey(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	if !errors.Is(err, ErrThreeDNoCredential) {
		t.Fatalf("非法 key 应拒绝：%v", err)
	}
}

func TestAutoThreeDCredentialsExpiryFailureAndIsolation(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	calls := map[string]int{}
	fail := true
	fetch := func(_ context.Context, a *auth.Auth) (string, time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[a.UID]++
		if fail {
			return "", time.Time{}, ErrThreeDNoCredential
		}
		if a.UID == "expiring" {
			return "ck_t_" + a.UID, time.Now().Add(autoKeyMargin / 2), nil
		}
		return "ck_t_" + a.UID, time.Time{}, nil
	}
	s := NewAutoThreeDCredentials(fetch, nil)
	if _, err := s.TempToken(ctx, &auth.Auth{UID: "u1"}); !errors.Is(err, ErrThreeDNoCredential) {
		t.Fatalf("initial failure err=%v", err)
	}
	fail = false
	for _, uid := range []string{"u1", "u2", "expiring", "expiring"} {
		if _, err := s.TempToken(ctx, &auth.Auth{UID: uid}); err != nil {
			t.Fatalf("%s: %v", uid, err)
		}
	}
	if calls["u1"] != 2 || calls["u2"] != 1 || calls["expiring"] != 2 {
		t.Fatalf("failure caching, UID isolation, or near-expiry refresh: %v", calls)
	}
	s.Invalidate("u1")
	if _, err := s.TempToken(ctx, &auth.Auth{UID: "u2"}); err != nil || calls["u2"] != 1 {
		t.Fatalf("invalidating u1 flushed u2: calls=%v err=%v", calls, err)
	}
}

func TestAutoThreeDCredentialsWaitingCallerCanCancel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := NewAutoThreeDCredentials(func(context.Context, *auth.Auth) (string, time.Time, error) {
		close(started)
		<-release
		return "ck_t_owner", time.Now().Add(time.Hour), nil
	}, nil)
	ownerDone := make(chan struct{})
	go func() {
		_, _ = s.TempToken(context.Background(), &auth.Auth{UID: "u1"})
		close(ownerDone)
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := s.TempToken(ctx, &auth.Auth{UID: "u1"})
		waiterDone <- err
	}()
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter remained blocked")
	}
	select {
	case <-ownerDone:
		t.Fatal("waiter cancellation interrupted the fetch owner")
	default:
	}
	close(release)
	<-ownerDone
}

func TestAutoThreeDCredentialsCacheAndFlight(t *testing.T) {
	calls := 0
	release := make(chan struct{})
	fetch := func(ctx context.Context, a *auth.Auth) (string, time.Time, error) {
		calls++
		<-release
		return "ck_t_" + a.UID, time.Now().Add(time.Hour), nil
	}
	s := NewAutoThreeDCredentials(fetch, nil)
	ctx := context.Background()
	a := &auth.Auth{UID: "u1"}

	go func() { time.Sleep(20 * time.Millisecond); close(release) }()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := s.TempToken(ctx, a)
			if err != nil || tok != "ck_t_u1" {
				t.Errorf("tok=%q err=%v", tok, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("并发同账号应单飞，签发 %d 次", calls)
	}
	// 命中缓存：不再签发。
	if _, err := s.TempToken(ctx, a); err != nil || calls != 1 {
		t.Fatalf("缓存未生效：calls=%d err=%v", calls, err)
	}
	// 失效后重签。
	s.Invalidate("u1")
	release2 := make(chan struct{})
	fetch2 := func(ctx context.Context, a *auth.Auth) (string, time.Time, error) {
		calls++
		<-release2
		return "ck_t_u1-new", time.Now().Add(time.Hour), nil
	}
	s.fetch = fetch2
	go func() { time.Sleep(20 * time.Millisecond); close(release2) }()
	if tok, err := s.TempToken(ctx, a); err != nil || tok != "ck_t_u1-new" {
		t.Fatalf("失效后应重签：tok=%q err=%v", tok, err)
	}
}

func TestAutoThreeDCredentialsManualPriority(t *testing.T) {
	fetchCalls := 0
	manual := NewStaticThreeDCredentials(map[string]string{"u1": "ck_t_manual"})
	s := NewAutoThreeDCredentials(func(context.Context, *auth.Auth) (string, time.Time, error) {
		fetchCalls++
		return "", time.Time{}, ErrThreeDNoCredential
	}, manual)
	ctx := context.Background()
	if tok, err := s.TempToken(ctx, &auth.Auth{UID: "u1"}); err != nil || tok != "ck_t_manual" {
		t.Fatalf("手工表优先：tok=%q err=%v", tok, err)
	}
	if fetchCalls != 0 {
		t.Fatalf("手工命中不该签发，实际 %d 次", fetchCalls)
	}
	// 换成别的账号：手工表没有 → 走签发（这里直接失败）。
	if _, err := s.TempToken(ctx, &auth.Auth{UID: "u9"}); !errors.Is(err, ErrThreeDNoCredential) {
		t.Fatalf("err=%v", err)
	}
	// 面板保存热替换手工表后，u9 命中新表，不再触碰签发。
	s.ReplaceManual(map[string]string{"u9": "ck_t_hot"})
	if tok, err := s.TempToken(ctx, &auth.Auth{UID: "u9"}); err != nil || tok != "ck_t_hot" {
		t.Fatalf("热替换后：tok=%q err=%v", tok, err)
	}
	if fetchCalls != 1 {
		t.Fatalf("热替换路径不该签发，实际 %d 次", fetchCalls)
	}
}

func TestThreeDKeyExpiresAt(t *testing.T) {
	if threeDKeyExpiresAt("2099-01-02T03:04:05Z").IsZero() {
		t.Fatal("RFC3339 应可解析")
	}
	if threeDKeyExpiresAt(float64(4102444800)).IsZero() {
		t.Fatal("unix 秒应可解析")
	}
	if threeDKeyExpiresAt(float64(4102444800000)).IsZero() {
		t.Fatal("unix 毫秒应可解析")
	}
	if !threeDKeyExpiresAt("garbage").IsZero() {
		t.Fatal("垃圾值应返回零值")
	}
	if !threeDKeyExpiresAt(nil).IsZero() {
		t.Fatal("缺字段应返回零值")
	}
}

// 上游鉴权拒绝后，同账号的两次完整查询必须使用不同的临时凭证。
func TestThreeDRejectInvalidatesAutoCache(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		code        string
		wantRefetch bool
	}{
		{name: "http 401", status: 401, code: "AuthFailure.Signature", wantRefetch: true},
		{name: "http 403", status: 403, code: "Forbidden", wantRefetch: true},
		{name: "business auth failure", status: 200, code: "AuthFailure", wantRefetch: true},
		{name: "bad request auth failure", status: 400, code: "AuthFailure.Signature", wantRefetch: true},
		{name: "ordinary parameter error", status: 400, code: "InvalidParameter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			fetchCalls, upstreamCalls := 0, 0
			auto := NewAutoThreeDCredentials(func(context.Context, *auth.Auth) (string, time.Time, error) {
				mu.Lock()
				defer mu.Unlock()
				fetchCalls++
				return fmt.Sprintf("ck_t_fake_%d", fetchCalls), time.Now().Add(time.Hour), nil
			}, nil)
			c := newThreeDClient(t, nil, rtFunc(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				upstreamCalls++
				call := upstreamCalls
				mu.Unlock()
				if call == 1 {
					body := fmt.Sprintf(`{"Response":{"Error":{"Code":%q,"Message":"fake rejection"}}}`, tc.code)
					return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				want := fmt.Sprintf("hy-3d.ck_t_fake_%d/", map[bool]int{true: 2, false: 1}[tc.wantRefetch])
				if !strings.Contains(r.Header.Get("Authorization"), want) {
					t.Errorf("second call did not use expected credential generation")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"Response":{"Status":"RUN"}}`))}, nil
			}))
			c.ThreeDCredential = auto
			a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
			if _, err := c.ThreeDTask(context.Background(), a, "job"); err == nil {
				t.Fatal("rejection must be returned")
			}
			if _, err := c.ThreeDTask(context.Background(), a, "job"); err != nil {
				t.Fatal(err)
			}
			wantCalls := map[bool]int{true: 2, false: 1}[tc.wantRefetch]
			if fetchCalls != wantCalls {
				t.Fatalf("fetch calls=%d want %d", fetchCalls, wantCalls)
			}
		})
	}
}
