package upstream

// /v2/report 桌面指纹对齐 5.9.3（上游 2026-10-28 `ee1c80d` 第 9 处）。
//
// 旧值 ideVersion 5.5.6 / commit 5f9692… / releaseDate 1789036585355 与当前客户端
// 差一个大版本 —— 上报事件里的版本四元组本身就是可判别指纹。

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestDesktopFingerprint593(t *testing.T) {
	fp := desktopFingerprint(&auth.Auth{UID: "u1"})
	want := map[string]string{
		"ideVersion": "5.9.3",
		"extVersion": "5.9.3",
		"commit":     "d46924219e79332f49c37ccebf27292f87001b19",
	}
	for k, v := range want {
		if got, _ := fp[k].(string); got != v {
			t.Errorf("%s=%q want %q", k, got, v)
		}
	}
	if got, _ := fp["releaseDate"].(int64); got != 1793093056315 {
		t.Errorf("releaseDate=%d want 1793093056315", got)
	}
	// 机器/会话 id 仍按账号稳定派生（body 里本来就该有，与头不同）。
	if fp["machineId"] == "" || fp["sessionId"] == "" {
		t.Error("machineId/sessionId 不该为空")
	}
}
