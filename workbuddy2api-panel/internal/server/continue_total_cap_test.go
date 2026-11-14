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
	// 请求带了限额（highClientBody 的 max_tokens=2048），所以走 maxContinueSegments 这一档；
	// 没有 usage 时 totalOutput 恒 0，累计上限判不到顶，段数上限就是唯一收口。
	if len(*reqs) != maxContinueSegments-1 {
		t.Fatalf("续写请求数=%d want %d（带限额按段数上限收口）", len(*reqs), maxContinueSegments-1)
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Fatalf("应以截断终态收尾:\n%s", out)
	}
}

// TestOutputCapByLimitAndFloor 累计输出上限只管带限额的请求：单段有效额度 = max(限额, 下限)，
// 两者都没有时用兜底值；不带限额的请求不按 token 封顶（段数管，见 noLimitContinueSegments）。
func TestOutputCapByLimitAndFloor(t *testing.T) {
	for _, c := range []struct {
		name     string
		limitSet bool
		limit    int64
		floor    int64
		want     float64
	}{
		{"不带限额：不按 token 封顶", false, 0, upstream.MinOutputBudget("low"), 0},
		{"限额小于下限：按下限", true, 2048, 8192, 8192 * continueTotalCapRatio},
		{"长文限额大于下限：按限额", true, 32000, 4096, 32000 * continueTotalCapRatio},
		{"限额不可解析且无下限：兜底", true, 0, 0, defaultContinueTotalCap},
	} {
		r := &continueReader{limitSet: c.limitSet, limit: c.limit, budgetFloor: c.floor}
		if got := r.outputCap(); got != c.want {
			t.Errorf("%s: outputCap=%v want %v", c.name, got, c.want)
		}
	}
}

// TestContinueNoLimitLongAnswerNotCappedByFloor 不带限额的长文（Codex 这类客户端）每段都能写到
// 上游单段上限（实测 32000）。累计上限若按「档位下限 × 4」算（low 档 16384、high 档 32768），
// 一段就超了：low/medium 档一次都续不了、high 档只能续一次——比只按段数封顶时续得更少。
// 不论档位，都应续满 noLimitContinueSegments 段，且只有一个终态。
func TestContinueNoLimitLongAnswerNotCappedByFloor(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "max"} {
		up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
			return 200, cutSegment(fmt.Sprintf("s%d", call), 32000)
		})
		r := newContinueReaderWithFloor(context.Background(), up,
			&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
			[]byte(`{"model":"cn:deepseek-v4.1-flash","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
			"1.2.3.4", upstream.ChatMeta{},
			io.NopCloser(strings.NewReader(cutSegment("s0", 32000))), upstream.MinOutputBudget(effort))
		out := readReader(t, r)
		if len(*reqs) != noLimitContinueSegments-1 {
			t.Errorf("effort=%q: 续写请求数=%d want %d", effort, len(*reqs), noLimitContinueSegments-1)
		}
		if n := strings.Count(out, `"finish_reason"`); n != 1 {
			t.Errorf("effort=%q: 终态帧=%d want 1:\n%s", effort, n, out)
		}
	}
}

// TestContinueLongAnswerLimitAtLowEffort 客户端额度 ≥ clientLimitLongAnswer 按「要长文」续写；
// 低档位下累计上限若只按下限算（4096×4=16384），首段 32000 就到顶，这条例外形同虚设。
func TestContinueLongAnswerLimitAtLowEffort(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		return 200, cutSegment(fmt.Sprintf("s%d", call), 32000)
	})
	r := newContinueReaderWithFloor(context.Background(), up,
		&auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999},
		[]byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":32000,"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`),
		"1.2.3.4", upstream.ChatMeta{},
		io.NopCloser(strings.NewReader(cutSegment("s0", 32000))), upstream.MinOutputBudget("low"))
	out := readReader(t, r)
	// 上限 32000×4 = 128000 → 1 原生段 + 3 续写段。
	if len(*reqs) != 3 {
		t.Fatalf("续写请求数=%d want 3（额度 ≥32k 的请求按限额算累计上限）", len(*reqs))
	}
	if n := strings.Count(out, `"finish_reason"`); n != 1 {
		t.Fatalf("终态帧=%d want 1:\n%s", n, out)
	}
}
