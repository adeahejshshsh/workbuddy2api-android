package upstream

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 观察者只在**真实查到**余额后触发；失败不是观测值（否则抖动返回的 0 会在下次
// 成功时造出一条假变动）。
func TestNotifyCreditsOnlyOnSuccess(t *testing.T) {
	c := &Client{}
	var got []int64
	var uids []string
	c.SetCreditObserver(func(uid string, credits int64) {
		uids = append(uids, uid)
		got = append(got, credits)
	})
	a := &auth.Auth{UID: "u1"}

	c.notifyCredits(a, 100, nil)
	if len(got) != 1 || got[0] != 100 || uids[0] != "u1" {
		t.Fatalf("成功时应通知一次: %v %v", uids, got)
	}
	c.notifyCredits(a, 0, errFake{})
	if len(got) != 1 {
		t.Fatalf("失败不该通知，得到 %d 次", len(got))
	}
	c.notifyCredits(nil, 5, nil)
	if len(got) != 1 {
		t.Fatal("a 为 nil 不该通知")
	}
}

// 观察者是纯旁路：它 panic 不能打断余额刷新/签到主流程。
func TestNotifyCreditsSwallowsPanic(t *testing.T) {
	c := &Client{}
	c.SetCreditObserver(func(string, int64) { panic("boom") })
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("观察者 panic 不该外溢: %v", r)
		}
	}()
	c.notifyCredits(&auth.Auth{UID: "u1"}, 1, nil)
}

// 注销后不再触发；nil client 安全。
func TestNotifyCreditsUnsubscribe(t *testing.T) {
	c := &Client{}
	n := 0
	c.SetCreditObserver(func(string, int64) { n++ })
	c.SetCreditObserver(nil)
	c.notifyCredits(&auth.Auth{UID: "u1"}, 1, nil)
	if n != 0 {
		t.Fatalf("注销后仍触发 %d 次", n)
	}
	var nilClient *Client
	nilClient.SetCreditObserver(func(string, int64) { n++ })
	nilClient.notifyCredits(&auth.Auth{UID: "u1"}, 1, nil)
}

type errFake struct{}

func (errFake) Error() string { return "fake" }
