package pool

import (
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 备注必须有上限（审查 C5-04）：无上限时脚本灌超长备注会让 state.json 体积膨胀。
func TestSetRemarkTruncates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	long := strings.Repeat("注", remarkMaxRunes+50)
	if !p.SetRemark("u1", long) {
		t.Fatal("SetRemark 应成功")
	}
	st, _ := p.Status("u1")
	if n := len([]rune(st.Remark)); n != remarkMaxRunes {
		t.Fatalf("备注 rune 数=%d want %d", n, remarkMaxRunes)
	}
}

// 已禁用账号不再累计 12153 计数（审查 C5-07）。
func TestSessionDeadSkipsDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.Disable("u1", "测试禁用")
	for i := 0; i < sessionDeadThreshold+2; i++ {
		if p.NoteSessionDeadWith("u1", "test") {
			t.Fatal("已禁用账号不该再被「禁用一次」")
		}
	}
}

// 熔断时长配置缺失时不能静默失效（审查 C5-24）。
func TestBreakerCooldownZeroConfigStillBlocks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.breakerCooldown = 0
	p.breakerCooldownMax = 0
	p.breakerThreshold = 1
	e := p.byUID["u1"]
	p.mu.Lock()
	p.recordBreakerFailureLocked(e)
	until := e.breakerUntil
	p.mu.Unlock()
	if !until.After(time.Now().Add(time.Minute)) {
		t.Fatalf("熔断应在未来生效，实际 until=%v（配置为 0 时静默失效）", until)
	}
}
