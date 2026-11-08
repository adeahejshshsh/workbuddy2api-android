package server

import (
	"encoding/json"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestStarvedRetryBodyRaisesBudget 救援段必须带上预算下限。
//
// 线上实测（2026-11-08，cn:deepseek-v4.1-flash，effort=high、客户端 max_tokens=1024）：
// 首段被抬到 8192，但这 8192 全被思考吃掉、正文 0B → 触发救援；救援段从客户端**原始**
// body 重建，只拿到 1024，正文写 1946 字就断。这条钉住修复：救援段至少 floor。
func TestStarvedRetryBodyRaisesBudget(t *testing.T) {
	raw, ok := starvedRetryBodyWithFloor(
		[]byte(`{"model":"deepseek-v4.1-flash","max_tokens":1024,"reasoning_effort":"high","messages":[]}`),
		upstream.MinOutputBudget("high"))
	if !ok {
		t.Fatal("应构造出救援体")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := obj["max_tokens"].(float64); int64(got) != 8192 {
		t.Fatalf("救援段 max_tokens=%v want 8192（high 档下限）", obj["max_tokens"])
	}
	// 关思考的约定不变：救援靠的是「把预算从思考拿回来」。
	if th, _ := obj["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Fatalf("救援段应关思考，实际 thinking=%v", obj["thinking"])
	}
	if _, has := obj["reasoning_effort"]; has {
		t.Fatal("关思考时不得带档位")
	}
}

// TestStarvedRetryBodyKeepsLargeBudget 客户端额度本来就够大时不动它。
func TestStarvedRetryBodyKeepsLargeBudget(t *testing.T) {
	raw, ok := starvedRetryBodyWithFloor(
		[]byte(`{"model":"deepseek-v4.1-flash","max_tokens":65536,"thinking":{"type":"enabled"},"messages":[]}`),
		upstream.MinOutputBudget("max"))
	if !ok {
		t.Fatal("应构造出救援体")
	}
	var obj map[string]any
	json.Unmarshal(raw, &obj)
	if got, _ := obj["max_tokens"].(float64); int64(got) != 65536 {
		t.Fatalf("max_tokens=%v want 65536（不缩小客户端额度）", obj["max_tokens"])
	}
}

// TestContinuationBodyCarriesFloor 续写段同样带下限，且不放大档位对应的思考预算。
func TestContinuationBodyCarriesFloor(t *testing.T) {
	r := &continueReader{
		body:        []byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":2048,"reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`),
		budgetFloor: upstream.MinOutputBudget("max"),
		limitSet:    true,
	}
	r.text.WriteString("半句正文")
	r.segCompletion, r.segThink = 2048, 1900

	raw, err := r.continuationBody()
	if err != nil {
		t.Fatalf("continuationBody: %v", err)
	}
	var obj map[string]any
	json.Unmarshal(raw, &obj)
	if got, _ := obj["max_tokens"].(float64); int64(got) != 16384 {
		t.Fatalf("续写段 max_tokens=%v want 16384（max 档下限）", obj["max_tokens"])
	}
	// 思考吃光形态：续写段关思考（否则同一份额度再被烧一遍）。
	if th, _ := obj["thinking"].(map[string]any); th == nil || th["type"] != "disabled" {
		t.Fatalf("思考吃光的续写段应关思考，实际 thinking=%v", obj["thinking"])
	}
}

// TestMinOutputBudgetTable 下限表本身：未知档位返回 0（不猜），大小单调。
func TestMinOutputBudgetTable(t *testing.T) {
	if got := upstream.MinOutputBudget(""); got != 0 {
		t.Fatalf("未知档位应返回 0，got %d", got)
	}
	if got := upstream.MinOutputBudget("nonsense"); got != 0 {
		t.Fatalf("未知档位应返回 0，got %d", got)
	}
	prev := int64(0)
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		v := upstream.MinOutputBudget(e)
		if v <= prev {
			t.Fatalf("%s 的下限 %d 未随档位递增（prev=%d）", e, v, prev)
		}
		prev = v
	}
}
