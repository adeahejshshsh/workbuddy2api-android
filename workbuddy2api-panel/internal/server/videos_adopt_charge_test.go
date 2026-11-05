package server

import "testing"

// 收养时的记账语义：已到终态的收养条目视为已记账，还在跑的保持未记账。
//
// 这条不能只靠「查一次已完成任务积分是 0」来钉：探针路径本身不过记账代码，
// 那种断言在 #177 之后结构性地恒绿。真正需要守住的是台账标记 —— 终态快照被
// gcLocked 清掉（12h）后同一个任务会重新走一遍真实查询，那时只有 Charged 能挡住重复记账。
func TestVideoAdoptChargeSemantics(t *testing.T) {
	done := newVideoTaskStore()
	done.adopt("t-done", "u1", "cn", "m", true)
	if done.claimCharge("t-done") {
		t.Fatal("收养时已到终态的任务不该再记一次账")
	}

	running := newVideoTaskStore()
	running.adopt("t-run", "u1", "cn", "m", false)
	if !running.claimCharge("t-run") {
		t.Fatal("收养时还在跑的任务出片时应记一次")
	}
	if running.claimCharge("t-run") {
		t.Fatal("同一任务只能记一次")
	}
}
