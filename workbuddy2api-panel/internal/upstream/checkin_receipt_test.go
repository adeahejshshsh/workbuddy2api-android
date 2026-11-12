// checkin_receipt_test.go 钉住签到回执的四态判定。
//
// 线上实测样本（2026-11-12 07:08 CST，CN 号，同一号连打两次）：
//
//	首次  HTTP 200 {"credit":100,"streak_days":44,"is_streak_day":false}
//	重复  HTTP 400 {"code":10001,"msg":"今天已签到，请明天再来","requestId":"…"}
//
// 回归点：
//   - 重复签到不能算「本次签到成功」（同类项目 hub#875：生产 1288 条记录里 250 条
//     幂等拒绝被报成「签到成功 (+100积分)」）；
//   - 「签到失败，已签到天数不足 7 天」同时含「失败」和「已签到」，必须判 failed，
//     不能被 already 抢走；
//   - 积分只能取上游回执里真实带回的字段，没带回时 CreditKnown 必须为假（调用方
//     据此不显示数字，而不是显示 0 或硬编码的 100）。
package upstream

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestCheckinReceiptClassifies(t *testing.T) {
	shortBillingRetry(t)
	cases := []struct {
		name   string
		status int
		body   string
		want   CheckinState
		credit int
		known  bool
	}{
		// 成功回执是平铺的（没有 {code,msg,data} 信封），积分必须从原始 body 里取。
		{"fresh flat", 200, `{"credit":100,"streak_days":44,"is_streak_day":false}`, CheckinChecked, 100, true},
		{"fresh enveloped", 200, `{"code":0,"msg":"OK","data":{"credit":100,"streak_days":12}}`, CheckinChecked, 100, true},
		{"fresh no credit field", 200, `{"code":0,"msg":"OK","data":{}}`, CheckinChecked, 0, false},
		{"fresh zero credit", 200, `{"code":0,"msg":"OK","data":{"credit":0}}`, CheckinChecked, 0, true},

		// 重复签到：本题的核心。
		{"repeat http400 10001", 400, `{"code":10001,"msg":"今天已签到，请明天再来","requestId":"x"}`, CheckinAlready, 0, false},
		{"repeat http200 10001", 200, `{"code":10001,"msg":"今日已签到，明天再来吧"}`, CheckinAlready, 0, false},
		// 另一版本上游把幂等拒绝放在 code=0 里（hub#875 生产实测），同样不能当成本次签到。
		{"repeat code0 wording", 200, `{"code":0,"msg":"今天已签到，请明天再来"}`, CheckinAlready, 0, false},

		// 「失败」是硬词，必须压过 already。
		{"failed streak short", 200, `{"code":10001,"msg":"签到失败，已签到天数不足 7 天"}`, CheckinFailed, 0, false},
		{"failed retry later", 400, `{"code":10001,"msg":"签到失败，请稍后重试"}`, CheckinFailed, 0, false},
		{"failed activity off", 400, `{"code":10001,"msg":"check-in activity not started"}`, CheckinFailed, 0, false},
		{"failed enterprise", 400, `{"code":10001,"msg":"企业账号不支持该操作"}`, CheckinFailed, 0, false},

		// 「请求处理中」是临时态：不是终态失败，可以稍后重试。
		{"pending processing", 429, `{"code":12000,"msg":"请求处理中，请勿重复操作"}`, CheckinPending, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(func(r *http.Request) (*http.Response, error) {
				return jsonResp(tc.status, tc.body), nil
			})
			rec, _ := c.CheckinReceipt(&auth.Auth{AccessToken: "at"})
			if rec.State != tc.want {
				t.Fatalf("state=%q want %q (code=%d msg=%q)", rec.State, tc.want, rec.Code, rec.Msg)
			}
			if rec.CreditKnown != tc.known || (tc.known && rec.Credit != tc.credit) {
				t.Fatalf("credit=%d known=%v want %d/%v", rec.Credit, rec.CreditKnown, tc.credit, tc.known)
			}
			if tc.want == CheckinAlready && rec.Msg == "" {
				t.Fatal("already 必须带回上游原文，面板要能显示「今天已签到」的依据")
			}
			if tc.want == CheckinAlready && rec.CreditKnown {
				t.Fatal("already 绝不能带积分：上游这次一个积分都没发")
			}
		})
	}
}

// TestDailyCheckinErrorSemanticsUnchanged 老调用方（调度器 / CLI）只看 error：
// checked/already 成功，pending/failed 失败，语义与改造前一致。
func TestDailyCheckinErrorSemanticsUnchanged(t *testing.T) {
	shortBillingRetry(t)
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"fresh", 200, `{"credit":100}`, false},
		{"repeat", 400, `{"code":10001,"msg":"今天已签到，请明天再来"}`, true},
		// 改造前 IsAlreadyCheckin 会把这条当幂等成功，面板因此显示「今日已签」；
		// 语义其实是失败（含「失败」硬词），必须仍然报错。
		{"failed streak short", 200, `{"code":10001,"msg":"签到失败，已签到天数不足 7 天"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(func(r *http.Request) (*http.Response, error) {
				return jsonResp(tc.status, tc.body), nil
			})
			err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestCheckinReceiptGlobalFallbackKeepsRaw 404 换路径的老语义不变：global 先打无 /v2
// 前缀，404 后回落 /v2，且回落成功时回执照常解析。
func TestCheckinReceiptGlobalFallbackKeepsRaw(t *testing.T) {
	shortBillingRetry(t)
	a := &auth.Auth{AccessToken: "at"}
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatalf("set realm: %v", err)
	}
	var paths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/billing/meter/daily-checkin" {
			return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
		}
		return jsonResp(200, `{"credit":50}`), nil
	})
	c.GlobalEnabled = true
	rec, err := c.CheckinReceipt(a)
	if err != nil {
		t.Fatalf("global 回落应成功: %v", err)
	}
	if len(paths) != 2 || rec.State != CheckinChecked || !rec.CreditKnown || rec.Credit != 50 {
		t.Fatalf("paths=%v rec=%+v", paths, rec)
	}
}
