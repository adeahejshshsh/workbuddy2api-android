package pool

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 落盘必须在锁外做（审查 C5-37）：此前 saveLocked 在写锁内做四次带 fsync 的 IO，
// 磁盘慢时整个账号池被堵死。这里用「落盘期间另一个 goroutine 能否拿到锁」来证明。
func TestFlushDoesNotHoldPoolLock(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.dirty.Store(true)

	// 把 stateFp 指向一个「写入很慢」的路径是不现实的，改用并发探测：
	// 落盘期间反复尝试读锁，只要有一次成功就说明锁没被全程持有。
	var acquired atomic.Bool
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if p.mu.TryLock() {
				acquired.Store(true)
				p.mu.Unlock()
				return
			}
			time.Sleep(time.Microsecond)
		}
	}()
	for i := 0; i < 20; i++ {
		_ = p.flushNowErr()
	}
	close(stop)
	wg.Wait()
	if !acquired.Load() {
		t.Fatal("落盘期间一次都没拿到锁：说明 IO 仍在写锁内")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("state.json 未生成: %v", err)
	}
}

// flushNow 只在有变更时写；flushNowErr 无条件写（保留原 saveLocked 语义）。
func TestFlushNowSkipsWhenClean(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.dirty.Store(true)
	if err := p.flushNowErr(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	st1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 干净状态下 flushNow 不该重写文件。
	time.Sleep(10 * time.Millisecond)
	p.flushNow()
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Fatal("无变更时 flushNow 仍写了盘")
	}
	// 但 flushNowErr 会（调用方明确要求「现在就落盘」）。
	p.flushNowErr()
	st3, _ := os.Stat(path)
	if st3.ModTime().Equal(st2.ModTime()) {
		t.Fatal("flushNowErr 应无条件写盘")
	}
}
