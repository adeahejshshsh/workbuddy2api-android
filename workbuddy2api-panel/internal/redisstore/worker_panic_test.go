package redisstore

import (
	"testing"
	"time"
)

// 写队列里的任务 panic 不能带走 worker（审查 C8）：worker 一死，workerWG 永不 Done，
// Close() 卡到超时，镜像写入从此静默停摆。
func TestWriteWorkerSurvivesPanic(t *testing.T) {
	// 零值 Upstash 也能用（init 懒初始化）：这里只需要写队列的语义。
	u := &Upstash{}
	u.init()

	ran := make(chan struct{}, 2)
	u.goWrite(func() { panic("boom") })
	u.goWrite(func() { ran <- struct{}{} })

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("第一个任务 panic 之后，后续任务没被执行（worker 已死）")
	}
	// Close 必须能在秒级完成（panic 的那个也要 wg.Done）。
	done := make(chan struct{})
	go func() { _ = u.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 卡住：panic 的任务没记 wg.Done")
	}
}
