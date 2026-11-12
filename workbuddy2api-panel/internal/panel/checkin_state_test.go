// checkin_state_test.go 钉住面板签到端点对外暴露的四态回执。
//
// 为什么要在 handler 层再钉一遍：判定逻辑在 upstream 里错了会造成「文案错」，但
// 这里缺字段（比如没把 checkin_state 塞进响应）会让前端无处可判、只能退回
// checkin_done 二选一，同样把幂等拒绝说成签到成功（hub#875）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 本仓响应（panel.go 的 accountCheckin）：ok 恒为 true（接口层成功），事实在
// checkin_state / checkin_done / checkin_message 里；积分只在真签到时出现。
type checkinResponse struct {
	OK      bool   `json:"ok"`
	Done    bool   `json:"checkin_done"`
	State   string `json:"checkin_state"`
	Message string `json:"checkin_message"`
	Credit  *int   `json:"checkin_credit"`
	Credits int64  `json:"credits"`
}

// checkinTestServer 只为 daily-checkin 返回 scripted 回执，余额查询固定成功。
func checkinTestServer(t *testing.T, status int, body string) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case "/v2/billing/meter/get-user-resource":
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":7,"CapacitySize":12}]}}}}`))
		default:
			t.Errorf("unexpected billing path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.HTTP, up.BillingBaseCN = srv.Client(), srv.URL
	return up
}

func doCheckin(t *testing.T, up *upstream.Client) checkinResponse {
	t.Helper()
	p := New(Config{Pool: poolOfOne(t), Upstream: up})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/accounts/cn-1/checkin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp checkinResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return resp
}

func poolOfOne(t *testing.T) *pool.Pool {
	t.Helper()
	p := pool.New("")
	// 空 realm + 空 domain → Realm() 判为 cn（realmLocked 的既有口径）。
	p.Add(&auth.Auth{UID: "cn-1", AccessToken: "at", ExpiresAt: 9999999999})
	return p
}

// TestCheckinAlreadyIsNotReportedAsFreshSuccess 重复签到：上游 HTTP 400 +
// code 10001「今天已签到，请明天再来」，本次一个积分都没发。
// 面板必须报 already、不得出现积分数字、不得当成新签到（checkin_done 仍为真，
// 因为「今天确实已签」这个事实成立）。
func TestCheckinAlreadyIsNotReportedAsFreshSuccess(t *testing.T) {
	up := checkinTestServer(t, http.StatusBadRequest,
		`{"code":10001,"msg":"今天已签到，请明天再来","requestId":"32b11f2c"}`)
	resp := doCheckin(t, up)
	if !resp.Done || resp.State != "already" || !resp.OK {
		t.Fatalf("resp=%+v want done=true state=already ok=true", resp)
	}
	if resp.Credit != nil {
		t.Fatalf("already 不得带积分，got %d", *resp.Credit)
	}
	if !strings.Contains(resp.Message, "今天已签到") {
		t.Fatalf("message=%q 应保留上游原文", resp.Message)
	}
	if resp.Done != true {
		t.Fatal("already 仍代表「今天确实已签」，done 必须为真")
	}
}

// TestCheckinCheckedCarriesRealCredit 真签到：积分只取上游回执里带回的数字。
func TestCheckinCheckedCarriesRealCredit(t *testing.T) {
	up := checkinTestServer(t, http.StatusOK, `{"credit":100,"streak_days":44,"is_streak_day":false}`)
	resp := doCheckin(t, up)
	if !resp.Done || !resp.OK || resp.State != "checked" {
		t.Fatalf("resp=%+v want done=true ok=true state=checked", resp)
	}
	if resp.Credit == nil || *resp.Credit != 100 {
		t.Fatalf("checkin_credit=%v want 100", resp.Credit)
	}
}

// TestCheckinCheckedWithoutCreditFieldStillOmitsNumber 上游没带回积分时不显示数字
// （而不是显示 0，更不是硬编码 100）。
func TestCheckinCheckedWithoutCreditFieldStillOmitsNumber(t *testing.T) {
	up := checkinTestServer(t, http.StatusOK, `{"code":0,"msg":"OK","data":{}}`)
	resp := doCheckin(t, up)
	if resp.State != "checked" || resp.Credit != nil {
		t.Fatalf("resp=%+v want state=checked 且无 checkin_credit", resp)
	}
}

// TestCheckinFailureWordsBeatAlready 语义为失败的回执不能被 already 抢走
// （「签到失败，已签到天数不足 7 天」同时含「失败」与「已签到」）。
func TestCheckinFailureWordsBeatAlready(t *testing.T) {
	up := checkinTestServer(t, http.StatusOK, `{"code":10001,"msg":"签到失败，已签到天数不足 7 天"}`)
	resp := doCheckin(t, up)
	if resp.State != "failed" || resp.Done {
		t.Fatalf("resp=%+v want state=failed done=false", resp)
	}
	if resp.Message == "" {
		t.Fatal("失败必须带 checkin_message 供前端展示原因")
	}
}

// pending（429 请求处理中）由内部重试阶梯覆盖，handler 层不重复跑 17 秒退避：
// 见 internal/upstream/checkin_receipt_test.go 的 pending 用例。
