package upstream

import "testing"

// IsAlreadyCheckin 是调度器与 CLI 判「幂等成功」的唯一出口：失败硬词必须压过「已签到」，
// 且不能凭业务码 10001 判定——10001 同样是「企业账号不支持该操作」等失败的码。
func TestIsAlreadyCheckinFailureWordsWin(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"今天已签到，请明天再来", true},
		{"今日已签到，明天再来吧", true},
		{"already checked in today", true},
		{"签到失败，已签到天数不足 7 天", false},
		{"http 400 code=10001 msg=企业账号不支持该操作", false},
		{"check-in activity not started", false},
		{"upstream error: code=10001 今天已签到", true},
		{"upstream error: code=400 msg=request illegal", false},
		{"http 500", false},
	} {
		if got := IsAlreadyCheckin(&Error{Kind: ErrClient, Msg: tc.msg}); got != tc.want {
			t.Errorf("IsAlreadyCheckin(%q)=%v want %v", tc.msg, got, tc.want)
		}
	}
}
