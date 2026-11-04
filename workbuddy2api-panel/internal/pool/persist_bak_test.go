package pool

// state.json 被外部截断/写坏时，load() 会回退 .bak —— 但磁盘上的主文件仍是残骸。
// 从 .bak 恢复后的**第一次落盘**（≤5 秒后）此前会把残骸写进 .bak，随后才写主文件；
// 若这次主文件写入失败（磁盘满、单文件 bind mount EBUSY），两份都是坏的，
// corrupt-* 也没生成 → 冷却截止、熔断计数、成本账本、credits、「已签到」全部不可恢复。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestSaveKeepsValidBakWhenMainFileCorrupt(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")

	// 先造一份**合法**状态落盘，拿到真实的 state.json 形态与它的 .bak。
	p := New(stateFp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetCredits("u1", 1234, 0)
	if err := p.saveLocked(); err != nil {
		t.Fatalf("首次落盘: %v", err)
	}
	// 再落一次，让 .bak 里也是一份合法内容。
	if err := p.saveLocked(); err != nil {
		t.Fatalf("第二次落盘: %v", err)
	}
	goodBak, err := os.ReadFile(stateFp + ".bak")
	if err != nil {
		t.Fatalf("读 .bak: %v", err)
	}
	if !json.Valid(goodBak) {
		t.Fatalf(".bak 前置条件应为合法 JSON")
	}

	// 把主文件写坏（模拟外部截断/断电残骸），再从它重建池（load 会回退 .bak）。
	if err := os.WriteFile(stateFp, []byte(`{"accounts":[{"uid":"u1"`), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(stateFp) // 解析失败 → 回退 .bak
	p2.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})

	// 恢复后的第一次落盘：**不许**把残骸覆盖进 .bak。
	if err := p2.saveLocked(); err != nil {
		t.Fatalf("恢复后落盘: %v", err)
	}
	after, err := os.ReadFile(stateFp + ".bak")
	if err != nil {
		t.Fatalf("读 .bak: %v", err)
	}
	if !json.Valid(after) {
		t.Fatalf(".bak 被残骸污染了（%d 字节，非法 JSON）—— 唯一的退路被毁", len(after))
	}
}
