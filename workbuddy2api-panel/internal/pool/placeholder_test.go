package pool

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// bug P1-3 回归：Redis 快照恢复出「只有 UID 的占位账号」，之后 SyncToDir 扫到 0 个
// 账号（auth_dir 配错/卷没挂）时直接 return 保护状态 —— 占位条目必须**不参与路由**，
// 否则 pick 会选中它，NeedsRefresh 因 ExpiresAt=0 恒真 → RefreshToken 报
// no refreshToken → 熔断+轮转：面板显示有号、每次请求全 503。
func TestSnapshotPlaceholderNotRoutableUntilCredsArrive(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	ms := &memStore{loadOK: true}
	snap := snapshot{
		stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 100}}},
		SavedAt:   time.Now(),
	}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()

	// 恢复出来的是占位：状态可见（credits 保留），但不可选。
	st, ok := p.Status("u1")
	if !ok || st.Credits != 100 {
		t.Fatalf("占位条目应保留状态: %+v ok=%v", st, ok)
	}
	if got := p.PickExcludingForRealm(nil, "", ""); got != nil {
		t.Fatalf("占位账号（无凭证）不该被选中，got uid=%q", got.UID)
	}
	p.SyncToDir(nil) // 扫到 0 个：保护性 return，不剔除
	if got := p.PickExcludingForRealm(nil, "", ""); got != nil {
		t.Fatalf("SyncToDir(0) 之后占位账号仍不该被选中，got uid=%q", got.UID)
	}

	// 真实凭证到位（upsert 覆盖）→ 立刻可选。
	p.SyncToDir([]*auth.Auth{{UID: "u1", AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour).Unix()}})
	if got := p.PickExcludingForRealm(nil, "", ""); got == nil || got.UID != "u1" {
		t.Fatalf("拿到凭证后应当可选，got=%v", got)
	}
}
