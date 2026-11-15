package redisstore

import (
	"testing"
)

// 零值 Upstash（测试直接构造）不能 panic（审查 C7-029）：此前 SetBind/DelBind/
// SaveState/LoadState/LoadBinds 直接解引用 u.client。
func TestZeroValueUpstashDoesNotPanic(t *testing.T) {
	u := &Upstash{}
	u.SetBind("k", "uid", 0)
	u.DelBind("k")
	u.SaveState([]byte(`{}`))
	if _, ok := u.LoadState(); ok {
		t.Fatal("零值 Upstash 不该读到状态")
	}
	if m := u.LoadBinds(); len(m) != 0 {
		t.Fatalf("零值 Upstash 不该读到绑定，实际 %d 条", len(m))
	}
	if err := u.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// 队列满时 SaveState 不能被丢弃（审查 C7-033）：它可能是停机前最后一次快照。
func TestSaveStateSurvivesFullQueue(t *testing.T) {
	u := &Upstash{}
	// 标记「已初始化」但不启动 worker：这样队列能保持满而不被消费。
	// （直接 init 会起 worker 消费，wg 计数就对不上了。）
	u.initOnce.Do(func() {})
	u.done = make(chan struct{})
	u.q = make(chan func(), 1)
	u.q <- func() {} // 塞满

	u.SaveState([]byte(`{"x":1}`))

	if u.Dropped() != 0 {
		t.Fatalf("SaveState 被计入丢弃（%d）：状态快照不能走丢弃路径", u.Dropped())
	}
	// 对比：普通 SetBind 在队列满时仍走丢弃（尽力而为语义不变）。
	u.SetBind("k", "uid", 0)
	if u.Dropped() != 1 {
		t.Fatalf("SetBind 应计入丢弃，实际 Dropped=%d", u.Dropped())
	}
}

// 写失败要计数并暴露（审查 C7-031）：此前只有 debug 日志，面板分不清「丢弃」与「写失败」。
func TestFailedCounterExposed(t *testing.T) {
	u := &Upstash{}
	if u.Failed() != 0 {
		t.Fatal("初始应为 0")
	}
	u.noteWriteFail("SetBind", "k", errTest{})
	if u.Failed() != 1 {
		t.Fatalf("Failed()=%d want 1", u.Failed())
	}
}

type errTest struct{}

func (errTest) Error() string { return "test error" }
