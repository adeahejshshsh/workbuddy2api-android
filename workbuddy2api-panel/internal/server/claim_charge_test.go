package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 台账条目被淘汰后，同一任务不能重新赢得记账权（审查 B5）：否则重复扣费。
func TestClaimChargeNoDoubleChargeAfterEviction(t *testing.T) {
	s := newVideoTaskStore()
	s.put("t1", "u1", "cn", "m")
	if !s.claimCharge("t1") {
		t.Fatal("首次查询应赢得记账权")
	}
	if s.claimCharge("t1") {
		t.Fatal("第二次不该再记")
	}
	// 模拟 TTL/容量淘汰：条目没了。
	s.mu.Lock()
	delete(s.m, "t1")
	s.mu.Unlock()
	if s.claimCharge("t1") {
		t.Fatal("条目淘汰后不该重新赢得记账权（会重复扣费）")
	}
}

// 3D 同口径。
func TestThreeDClaimChargeNoDoubleChargeAfterEviction(t *testing.T) {
	s := newThreeDTaskStore()
	s.put("j1", "u1", "cn", "3.1", upstream.ThreeDState{JobID: "j1", Status: "DONE"})
	if !s.claimCharge("j1") {
		t.Fatal("首次查询应赢得记账权")
	}
	if s.claimCharge("j1") {
		t.Fatal("第二次不该再记")
	}
	s.mu.Lock()
	delete(s.m, "j1")
	s.mu.Unlock()
	if s.claimCharge("j1") {
		t.Fatal("条目淘汰后不该重新赢得记账权")
	}
}
