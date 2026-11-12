package panel

// 测试用的账号夹具：显式 realm 由 domain 表达（Auth.realm 不导出，也没有 setter）。
//
// 为什么需要：上游的 campaign / checkin 用例都按 realm 构造账号（cn-1 / global-1），
// 本仓没有它们那套 helper，各自写一遍会漂移——realm 判据（isGlobalDomain）一旦调整，
// 两处夹具只会改到一处。集中在这里，判据变了只改这份。

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func testRealmAuth(t *testing.T, uid, realm string) *auth.Auth {
	t.Helper()
	a := &auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999}
	if realm == "global" {
		// 与生产落盘值同形：国际站 domain（isGlobalDomain 认 .workbuddy.ai / codebuddy.ai）。
		a.Domain = "https://www.codebuddy.ai"
	}
	return a
}
