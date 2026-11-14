package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 上游只回 credit、不回 token 数时（3D / 部分视频）也必须下账（审查 B2）：
// 否则余额水位虚高、credit_floor 保底失真、costTier 选号被污染。
func TestNoteModelCostBooksCreditWithoutTokens(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.SetCredits("u1", 100, 100)
	p.NoteModelCost("u1", "hy-3d", 20, 0) // 20 积分、无 token 数
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在")
	}
	if st.Credits > 95 {
		t.Fatalf("credits=%d：tokens=0 时没下账（应扣掉 20）", st.Credits)
	}
}
