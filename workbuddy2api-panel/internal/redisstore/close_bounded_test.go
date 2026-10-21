package redisstore

import (
	"testing"
	"time"
)

// B1（BUG-REVIEW-2026-10-21）：worker 卡在慢写里时，Close 必须有界返回。
//
// 修之前：第一段 10s 超时只约束「等在途写排空」，紧接着无界的 workerWG.Wait()
// 又把耗时放开 —— 单个 worker 卡在一次慢写（Upstash 重连时可达 Read/WriteTimeout
// 量级）就能把 main 的 defer store.Close() 拖穿 run.sh 的 8s 停机预算，
// 最后 kill -9 把最后一次 Flush 夺走，正是 run.sh 注释里要避免的事。
func TestCloseBoundedWhenWorkerStuck(t *testing.T) {
	oldDrain, oldWorker := closeDrainTimeout, closeWorkerTimeout
	closeDrainTimeout, closeWorkerTimeout = 30*time.Millisecond, 30*time.Millisecond
	defer func() { closeDrainTimeout, closeWorkerTimeout = oldDrain, oldWorker }()

	u := &Upstash{}
	block := make(chan struct{})
	// 一笔永远不返回的写：模拟上游重连时卡死的 worker。
	u.goWrite(func() { <-block })

	start := time.Now()
	if err := u.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close 花了 %v：workerWG.Wait 仍在无界等待（两段超时都该已触发，应约 60ms）", d)
	}
	close(block) // 放行卡死的写，避免 goroutine 泄漏到其它测试
}
