package server

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// cutSegment 造一段「写满额度被截断」的上游流：正文 + finish=length + usage。
func cutSegment(id string, completion int) string {
	return fmt.Sprintf(`data: {"id":%q,"choices":[{"index":0,"delta":{"content":"段%s"}}]}`+"\n\n", id, id) +
		fmt.Sprintf(`data: {"id":%q,"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n", id, completion, completion+10) +
		"data: [DONE]\n\n"
}

func readReader(t *testing.T, r io.Reader) string {
	t.Helper()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

// TestContinueGoesBeyondThreeSegmentsWhenBudgetLeft 低档位写长文的回归：
//
// 线上实测（2026-11-08，影子实例 7870，effort=low + max_tokens=1024 → 下限 4096）：
// 每段正文约 4096 token，写「3000 字以上」的文档要 4 段以上才收尾，而当时的硬上限是
// 3 段——客户端在第 3 段末尾又看到半句（finish=length，尾巴停在「### 5.3 …- 正常请求」）。
// 段数不该是闸门：只要累计输出没到上限，就该接着续。
func TestContinueGoesBeyondThreeSegmentsWhenBudgetLeft(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, cutSegment(fmt.Sprintf("s%d", call), 4096)
	})
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(highClientBody), "1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(cutSegment("s0", 4096))), upstream.MinOutputBudget("low"))
	out := readReader(t, r)
	// 下限 4096 × 4 = 16384 累计上限 → 1 原生段 + 3 续写段。
	if len(*reqs) != 3 {
		t.Fatalf("续写请求数=%d want 3（旧实现的 2 次封顶就是低档位截断的根因）", len(*reqs))
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("撞累计上限时应以截断终态收尾:\n%s", out)
	}
}

// TestContinueStopsAtTotalOutputCap 累计输出到顶后不再续：这是开销的唯一硬闸门。
func TestContinueStopsAtTotalOutputCap(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, cutSegment(fmt.Sprintf("s%d", call), 4096)
	})
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(highClientBody), "1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(cutSegment("s0", 4096))), 4096)
	out := readReader(t, r)
	// 4 × 4096 = 16384 到顶，第 5 段不再发。
	if len(*reqs) > 3 {
		t.Fatalf("续写请求数=%d 超过累计上限（4096×4/4096=3）", len(*reqs))
	}
	if !r.continueCapped {
		t.Fatalf("到顶应置 continueCapped（日志要能区分「撞自己的上限」与「上游断流」）")
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("到顶后应以截断终态收尾:\n%s", out)
	}
}

// TestContinueNoUsageFallsBackToSegmentCeiling 上游不给 usage 时累计上限无法判定，
// 由段数上限兜底（不能无限续）。
func TestContinueNoUsageFallsBackToSegmentCeiling(t *testing.T) {
	noUsage := `data: {"id":"n","choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n" +
		`data: {"id":"n","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) { return 200, noUsage })
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(highClientBody), "1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(noUsage)), 4096)
	out := readReader(t, r)
	if len(*reqs) != maxContinueSegments-1 {
		t.Fatalf("续写请求数=%d want %d（无 usage 时按段数上限收口）", len(*reqs), maxContinueSegments-1)
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("应以截断终态收尾:\n%s", out)
	}
}

// TestOutputCappedDefaultsWithoutFloor 没算下限（客户端没指定档位）时用兜底上限。
func TestOutputCappedDefaultsWithoutFloor(t *testing.T) {
	r := &continueReader{usage: map[string]any{
		"completion_tokens": float64(defaultContinueTotalCap - 1),
	}}
	if r.outputCapped() {
		t.Fatalf("未到兜底上限不应判到顶")
	}
	r.usage["completion_tokens"] = float64(defaultContinueTotalCap)
	if !r.outputCapped() {
		t.Fatalf("到兜底上限应判到顶")
	}
	r2 := &continueReader{budgetFloor: 8192, usage: map[string]any{
		"completion_tokens": float64(8192 * continueTotalCapRatio),
	}}
	if !r2.outputCapped() {
		t.Fatalf("有下限时按 下限×%d 判顶", continueTotalCapRatio)
	}
}
