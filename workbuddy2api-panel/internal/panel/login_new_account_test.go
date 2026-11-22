package panel

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 回归：登录的是否新号必须在入池之前判，否则恒为 false，新号的后台任务永远不启动。
func TestSaveLoginAuthReportsNewAccount(t *testing.T) {
	e := newReloadEnv(t)
	now := time.Now().Add(24 * time.Hour).Unix()
	mk := func(at string) *auth.Auth {
		return &auth.Auth{UID: "u1", AccessToken: at, RefreshToken: "rt", ExpiresAt: now,
			FilePath: filepath.Join(e.dir, "workbuddy-u1.json")}
	}
	if isNew, err := e.panel.saveLoginAuth(mk("at1")); err != nil || !isNew {
		t.Fatalf("首次登录 isNew=%v err=%v want true/nil", isNew, err)
	}
	if isNew, err := e.panel.saveLoginAuth(mk("at2")); err != nil || isNew {
		t.Fatalf("重复登录 isNew=%v err=%v want false/nil", isNew, err)
	}
}
