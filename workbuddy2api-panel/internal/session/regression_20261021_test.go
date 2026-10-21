package session

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// bug 8：并发换号后，迟到的 touch 不得把会话写回旧账号。
//
// 时序：A 快路径命中 uid1 → B 因 uid1 失败轮转到 uid2 并 Bind → A 的续期迟到。
// 修之前 touch 无条件覆盖，会话被钉回那个刚失败的号（连 Redis 绑定一起写错）。
func TestTouchDoesNotOverwriteRebind(t *testing.T) {
	// 没有可用性钩子时 Router 会去问池子要候选集（本测试没池子）→ 一律判「不可用」，
	// 所以这里显式给一个「都可用」的钩子，把测试聚焦在 touch 的归属校验上。
	r := New(Config{
		TTL:                 time.Hour,
		Store:               redisstore.Noop{},
		IsAvailableForModel: func(model, uid string) bool { return uid != "" },
	})
	key := "sess-1"
	r.Bind(key, "uid-1")

	// 迟到的续期：声称自己是 uid-1
	r.touch(key, "uid-1", time.Now())
	if uid, ok := r.Resolve(key); !ok || uid != "uid-1" {
		t.Fatalf("touch 后 Resolve = (%q,%v)，期望 uid-1", uid, ok)
	}

	// 并发换号：会话已经绑到 uid-2
	r.Bind(key, "uid-2")

	// 旧请求的续期迟到：必须被拒绝
	r.touch(key, "uid-1", time.Now())
	if uid, ok := r.Resolve(key); !ok || uid != "uid-2" {
		t.Fatalf("迟到的 touch 把会话写回了旧号：Resolve=(%q,%v)，期望 uid-2", uid, ok)
	}
}
