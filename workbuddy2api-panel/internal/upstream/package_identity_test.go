package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// bug #4 回归：上游对账延迟给出 CycleCapacityUsed > CycleCapacitySize 时，
// 必须保持 remain+used == size 的恒等式 —— 否则面板出现
// 「剩余 50 / 已用 150 / 面额 100」这种自相矛盾的行，credit_floor 与选号权重也高估余额。
func TestPackageRemainUsedKeepsIdentityWhenUsedExceedsSize(t *testing.T) {
	cases := []struct {
		name       string
		in         respAccount
		wantRemain int64
		wantUsed   int64
		wantSize   int64
	}{
		{
			name:       "used 超过 size（脏数据）",
			in:         respAccount{CycleCapacitySize: 100, CycleCapacityRemain: 50, CycleCapacityUsed: 150},
			wantRemain: 0, wantUsed: 100, wantSize: 100,
		},
		{
			name:       "正常对账",
			in:         respAccount{CycleCapacitySize: 100, CycleCapacityRemain: 40, CycleCapacityUsed: 60},
			wantRemain: 40, wantUsed: 60, wantSize: 100,
		},
		{
			name:       "used 略大于 size-remain",
			in:         respAccount{CycleCapacitySize: 100, CycleCapacityRemain: 80, CycleCapacityUsed: 40},
			wantRemain: 60, wantUsed: 40, wantSize: 100,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remain, used, size := packageRemainUsed(c.in)
			if remain != c.wantRemain || used != c.wantUsed || size != c.wantSize {
				t.Fatalf("got remain=%d used=%d size=%d, want %d/%d/%d",
					remain, used, size, c.wantRemain, c.wantUsed, c.wantSize)
			}
			if remain+used != size {
				t.Fatalf("恒等式 remain+used==size 被破坏：%d+%d != %d", remain, used, size)
			}
		})
	}
}

// bug #5 回归：expiresIn 巨大时若"先乘后判"，int64 溢出成负数 → 校验通过 →
// ExpiresAt 落在过去 → NeedsRefresh 恒真 → 每个 chat 请求前都先打一次 refresh。
// 修复后这种脏值必须被忽略（ExpiresAt 保持旧值）。
func TestRefreshTokenIgnoresOverflowingExpiresIn(t *testing.T) {
	// 9.3e9 秒 ≈ 294 年：乘 time.Second 会溢出
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":9300000000}}`))
	}))
	defer srv.Close()

	future := time.Now().Add(2 * time.Hour).Unix()
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: future}
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessTokenValue() != "at-new" {
		t.Fatalf("token 应已更新: %q", a.AccessTokenValue())
	}
	if a.ExpiresAtValue() != future {
		t.Fatalf("溢出的 expiresIn 必须被忽略、保留旧 ExpiresAt；got %d want %d（落在过去会让每个请求都刷 token）",
			a.ExpiresAtValue(), future)
	}
	if time.Now().Unix() >= a.ExpiresAtValue() {
		t.Fatal("ExpiresAt 落在过去")
	}
}

// 对照：正常 expiresIn 仍要生效。
func TestRefreshTokenAppliesReasonableExpiresIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`))
	}))
	defer srv.Close()

	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 1}
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := time.Now().Add(time.Hour).Unix()
	if d := a.ExpiresAtValue() - want; d < -60 || d > 60 {
		t.Fatalf("ExpiresAt=%d 与 now+1h 偏差过大", a.ExpiresAtValue())
	}
	_ = json.Marshal // 保持 import 使用（若后续删用例）
}
