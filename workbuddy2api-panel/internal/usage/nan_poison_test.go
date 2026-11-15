package usage

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// 一个 NaN 不能让 usage.json 永久写不进去（审查 P2 数值毒化）。
func TestFlushSurvivesNaN(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u1", "m1", Delta{Credit: math.NaN(), HasCredit: true}, true)
	// 直接把毒值塞进桶（模拟上游/统计路径算出 NaN）。
	r.mu.Lock()
	for _, b := range r.buckets {
		b.TPS = math.NaN() // 速率均值是最容易被算出 NaN 的字段
	}
	r.mu.Unlock()

	r.flush(true) // 不应 panic、不应留下「永远失败」的状态
	r.mu.Lock()
	failed := r.failedFlushes
	r.mu.Unlock()
	if failed > 0 {
		t.Fatalf("非有限数值应被清零后成功落盘，实际失败 %d 次", failed)
	}
}

// scrubNonFinite 把非有限浮点归零，且不破坏结构。
func TestScrubNonFinite(t *testing.T) {
	in := map[string]any{"a": math.Inf(1), "b": []any{math.NaN(), 1.5}, "c": "x"}
	out := scrubNonFinite(in).(map[string]any)
	if out["a"].(float64) != 0 {
		t.Fatalf("+Inf 未清零: %v", out["a"])
	}
	if arr := out["b"].([]any); arr[0].(float64) != 0 || arr[1].(float64) != 1.5 {
		t.Fatalf("数组未清理: %v", arr)
	}
	if out["c"].(string) != "x" {
		t.Fatalf("字符串被破坏: %v", out["c"])
	}
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("清理后仍不可序列化: %v", err)
	}
}
