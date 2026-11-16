package upstream

// /v2/report 桌面指纹对齐 5.11.3（上游 2026-10-28 `ee1c80d` 第 9 处 + 2026-11-16 跟版）。
//
// 三元组是打包期常量，只能跟着分发包重采；旧值 5.5.6 / 5f9692… 差一个大版本，
// 5.9.3 那组也已过时 —— 上报事件里的版本四元组本身就是可判别指纹。

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestDesktopFingerprint593(t *testing.T) {
	fp := desktopFingerprint(&auth.Auth{UID: "u1"})
	want := map[string]string{
		"ideVersion": "5.11.3",
		"extVersion": "5.11.3",
		"commit":     "ecace0f7028166695994300182295329b8b95449",
	}
	for k, v := range want {
		if got, _ := fp[k].(string); got != v {
			t.Errorf("%s=%q want %q", k, got, v)
		}
	}
	if got, _ := fp["releaseDate"].(int64); got != 1794502169764 {
		t.Errorf("releaseDate=%d want 1794502169764", got)
	}
	// 机器/会话 id 仍按账号稳定派生（body 里本来就该有，与头不同）。
	if fp["machineId"] == "" || fp["sessionId"] == "" {
		t.Error("machineId/sessionId 不该为空")
	}
}
