package upstream

// 缓存过期瞬间有探测在飞时，此前直接返回 nil —— nil 在调用方看来等于「该域没有任何
// 模型」，一次并发就会让 /v1/models 短暂丢掉全部 global 条目。有上一轮成功缓存就
// 先用它顶着。

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestGlobalModelsReturnsStaleWhileProbing(t *testing.T) {
	c := New()
	c.GlobalEnabled = true
	// 造一份「过期但有内容」的缓存 + 标记有探测在飞。
	c.globalModels.Lock()
	c.globalModels.names = []string{"glm-5.2", "hy-image-3.5"}
	c.globalModels.infos = []ModelInfo{{ID: "glm-5.2"}}
	c.globalModels.fetched = time.Now().Add(-2 * time.Hour) // 已过 TTL
	c.globalModels.probing = true
	c.globalModels.Unlock()

	a := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	names, infos := c.fetchGlobalModelsOnce(a)
	if len(names) != 2 {
		t.Fatalf("探测在飞时应返回上一轮缓存，实际 names=%v（返回 nil 会让列表瞬间空掉）", names)
	}
	if len(infos) != 1 {
		t.Fatalf("infos 也应一并返回，实际 %v", infos)
	}
}

// 没有任何缓存时仍返回 nil（不编造）。
func TestGlobalModelsNilWhenProbingWithoutCache(t *testing.T) {
	c := New()
	c.GlobalEnabled = true
	c.globalModels.Lock()
	c.globalModels.probing = true
	c.globalModels.Unlock()

	a := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	if names, _ := c.fetchGlobalModelsOnce(a); names != nil {
		t.Fatalf("无缓存时应返回 nil，实际 %v", names)
	}
}
