package proxypool

// W4-09：状态锁内的 fsync 必须搬到锁外，且提交顺序不能乱。
//
// 原实现 commitLocked 在 m.mu 内做 json.MarshalIndent + MkdirAll +
// safefile.WriteKeep（写 + fsync + rename）。m.mu 同时是 selectLocked（每请求
// 热路径）与 Snapshot（面板轮询）的锁 —— 磁盘慢一次就把整个网关拖住几十毫秒。
//
// 修法：writeMu 作提交顺序锁（锁序 writeMu → mu），锁内只改状态 + 序列化，
// 锁外落盘，成功后才发布 m.data（保持「写失败不改运行态」的旧语义）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestCommitWritesOutsideManagerLock W4-09 的核心判据：落盘开始那一刻，
// 状态锁必须是**空闲**的。
//
// 变异验证：把 WriteKeep 挪回 m.mu 内（锁内落盘）→ 钩子里 TryLock 失败 → 本用例红。
func TestCommitWritesOutsideManagerLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.json")
	m := testManager(t, path)
	addProxy(t, m, "http://localhost:8080")

	locked := make(chan bool, 1)
	hook := func() {
		ok := m.mu.TryLock()
		if ok {
			m.mu.Unlock()
		}
		locked <- ok
	}
	m.commitHook.Store(&hook)

	if err := m.Bind("u1", Binding{Mode: "direct"}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !<-locked {
		t.Fatal("落盘时仍持有 m.mu：fsync 又回到了状态锁内（W4-09 回归，热路径会被磁盘拖死）")
	}
}

// TestConcurrentCommitsKeepAllBindings W4-09 的顺序判据：并发提交后，最终文件
// 必须包含**全部**成功返回的绑定。
//
// 变异验证：去掉 writeMu（锁外各自落盘、无提交序）→ 旧快照可能最后落盘，
// 文件里会缺条目，本用例红（32 路并发下几乎必然发生）。
func TestConcurrentCommitsKeepAllBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.json")
	m := testManager(t, path)
	addProxy(t, m, "http://localhost:8080")

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := m.Bind(fmt.Sprintf("u%02d", i), Binding{Mode: "direct"}); err != nil {
				t.Errorf("bind u%02d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读落盘文件: %v", err)
	}
	var d disk
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if len(d.Bindings) != n {
		t.Fatalf("落盘绑定数 = %d，want %d：旧提交覆盖了新提交（提交顺序丢失）", len(d.Bindings), n)
	}
}
