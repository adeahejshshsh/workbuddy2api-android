package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestGetGiftStatusPathAndShape 礼包状态：路径**无 /v2 前缀**（带 /v2 上游 404），
// 字段按线上实测 shape 解析。
func TestGetGiftStatusPathAndShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/meter/check-gift-claimed" {
			t.Errorf("path=%s want /billing/meter/check-gift-claimed（无 /v2 前缀）", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"claimed":true,` +
			`"claimed_at":"2026-09-24 18:15:15","active":true,"credit_num":1500,` +
			`"validity_period":1,"start_time":"2026-04-01 00:00:00","end_time":"2026-10-31 23:59:59"}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	st, err := c.GetGiftStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !st.Active || !st.Claimed || st.CreditNum != 1500 || st.EndTime != "2026-10-31 23:59:59" {
		t.Fatalf("解析不符: %+v", st)
	}
	if st.Pending() {
		t.Fatal("已领过不得判为待领（盲领会据此打出假 +0c 成功行）")
	}
}

// TestGiftPendingThreeWayAnd 三与门逐条判别：任一条不成立即「无需领取」。
func TestGiftPendingThreeWayAnd(t *testing.T) {
	cases := []struct {
		name string
		st   BenefitStatus
		want bool
	}{
		{"可领未领", BenefitStatus{Active: true, Claimed: false, CreditNum: 1000}, true},
		{"无活动", BenefitStatus{Active: false, Claimed: false, CreditNum: 1000}, false},
		{"已领过", BenefitStatus{Active: true, Claimed: true, CreditNum: 1000}, false},
		{"活动在但无额度", BenefitStatus{Active: true, Claimed: false, CreditNum: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.st.Pending(); got != c.want {
				t.Fatalf("Pending()=%v want %v", got, c.want)
			}
		})
	}
}

// TestGetGiftStatusNoActivityIsNotError active=false 是正常 200（无活动），
// 必须当成「无需领取」而不是错误——否则没活动的账号每天刷一片错误。
func TestGetGiftStatusNoActivityIsNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"claimed":false,"active":false,"credit_num":0}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	st, err := c.GetGiftStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("无活动不得报错: %v", err)
	}
	if st.Pending() {
		t.Fatal("active=false 不得判为待领")
	}
}

// TestGetGiftStatusErrorsOnHTTPFailure 真失败要显形：非 2xx 必须返回 error。
func TestGetGiftStatusErrorsOnHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":10001,"msg":"unauthorized"}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GetGiftStatus(&auth.Auth{UID: "u1", AccessToken: "at"}); err == nil {
		t.Error("401 必须返回 error（失败不得静默）")
	}
}
