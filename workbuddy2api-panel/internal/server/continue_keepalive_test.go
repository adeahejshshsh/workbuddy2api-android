// continue_keepalive_test.go 换段等待期不许静默（「wb 会中断、Trae 不中断」的协议差异）。
//
// 现象：同一客户端走 Trae 网关不中断，走本网关会中断。差别在换段：续写/救援段是一次
// 全新的上游 POST（十万级 prompt 的 prefill 常 3~15s），旧实现是**同步**发起的，
// 这段时间 Read() 被卡住，客户端一个字节都收不到；流式客户端有 idle 超时，静默到阈值
// 就主动断开——表现就是「答案说到一半突然中断」。
//
// 修法：换段请求异步发起，等待期内按 keepaliveEvery 补 ": keepalive" 注释帧。
// 本文件钉住五件事：
//  1. 等待期真的有保活，且客户端可见间隔不超过阈值；
//  2. 终态仍然只有一个（中间段的 finish 照旧扣下）；
//  3. 换段失败仍按首段终态收尾、不挂住；
//  4. 首段一个数据帧都没交出时**不补**保活——那会把 200 提交掉，首帧否决就换不了号；
//  5. 在途换段时 Close：请求被取消、迟到的响应体被收走关掉；
//  6. 换段失败落到决策链的下一条（与同步发起时一致）。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// testContinueBody 无限额请求体：首段就撞 length 时按截断续写。
// 注意不要带 max_tokens：带额度 = 客户端只要这么长，canContinue 会按预期拒绝续写。
const testContinueBody = `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`

// slowSegUpstream 第 1 次续写请求先睡 delay 再返回（模拟上游 prefill 很慢）。
// 首段是手工喂给 reader 的（不经 fake），所以 call=1 就是第一次换段请求。
func slowSegUpstream(t *testing.T, delay time.Duration, second string, secondStatus int) (*upstream.Client, *[]string) {
	t.Helper()
	return fakeContinueUpstream(t, func(call int) (int, string) {
		if call == 1 {
			time.Sleep(delay)
			return secondStatus, second
		}
		return 200, testSeg2Text
	})
}

// readTimeline 读完整个流，返回每次 Read 拿到的时间戳与内容。
func readTimeline(r io.Reader) ([]time.Time, []string) {
	var stamps []time.Time
	var chunks []string
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			stamps = append(stamps, time.Now())
			chunks = append(chunks, string(buf[:n]))
		}
		if err != nil {
			return stamps, chunks
		}
	}
}

// TestContinueKeepaliveWhileOpeningSegment 换段等待 1.5s：期间必须持续有保活帧，
// 且客户端可见的最大间隔不超过 keepaliveEvery 的量级。
func TestContinueKeepaliveWhileOpeningSegment(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 200 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })

	up, _ := slowSegUpstream(t, 1500*time.Millisecond, testSeg2Text, 200)
	r := newTestContinueReader(t, up, testContinueBody, io.NopCloser(strings.NewReader(testSeg1Text)))

	start := time.Now()
	stamps, chunks := readTimeline(r)
	elapsed := time.Since(start)

	all := strings.Join(chunks, "")
	if !strings.Contains(all, "继续") {
		t.Fatalf("续写段正文没接上: %q", all)
	}
	if strings.Count(all, `"finish_reason":"stop"`) != 1 {
		t.Fatalf("终态必须恰好一个, got %d: %q", strings.Count(all, `"finish_reason":"stop"`), all)
	}
	if strings.Contains(all, `"finish_reason":"length"`) {
		t.Fatalf("首段的 length 不该外泄: %q", all)
	}
	if n := strings.Count(all, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] 必须恰好一条, got %d: %q", n, all)
	}

	// 等待期保活：1.5s 的窗口里按 200ms 一条，至少 4 条。
	ka := strings.Count(all, ": keepalive")
	if ka < 4 {
		t.Fatalf("换段等待期保活帧=%d want >=4（否则客户端 1.5s 零字节，会被 idle 超时掐断）: %q", ka, all)
	}

	// 客户端可见间隔：最长的两帧间隔不该接近整个等待期。
	maxGap := time.Duration(0)
	prev := start
	for _, ts := range stamps {
		if d := ts.Sub(prev); d > maxGap {
			maxGap = d
		}
		prev = ts
	}
	if maxGap > 600*time.Millisecond {
		t.Fatalf("客户端最长静默 %v 超过阈值（保活没生效）", maxGap)
	}
	if elapsed < 1400*time.Millisecond {
		t.Fatalf("测试没真的等待换段（elapsed=%v），断言无效", elapsed)
	}
}

// TestContinueSegmentFailureStillSingleTerminal 换段失败（500）时：
// 按首段终态收尾、保活照旧、不多出终态、不挂住。
func TestContinueSegmentFailureStillSingleTerminal(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 100 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })

	up, _ := slowSegUpstream(t, 400*time.Millisecond, "", 500)
	r := newTestContinueReader(t, up, testContinueBody, io.NopCloser(strings.NewReader(testSeg1Text)))

	done := make(chan struct{})
	var stamps []time.Time
	var chunks []string
	go func() {
		stamps, chunks = readTimeline(r)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("换段失败后流挂住了（收尾没走到）")
	}

	all := strings.Join(chunks, "")
	if strings.Count(all, `"finish_reason"`) != 1 {
		t.Fatalf("换段失败后应恰好一个终态: %q", all)
	}
	if !strings.Contains(all, ": keepalive") {
		t.Fatalf("失败等待期也该有保活: %q", all)
	}
	if len(stamps) == 0 {
		t.Fatal("没有任何输出")
	}
}

// TestContinuePendingKeepsHeadVetoWindow 首段一帧都没交给客户端（唯一的数据帧就是
// finish=length 终态，被扣在 tail 里）时触发的空截断救援，等待期补的保活注释会被
// StreamHint 当作首行写出、提交 200——之后救援段首帧报错也换不了号，只能以
// 「200 + error 帧」收尾。客户端还没收到任何数据帧之前不该补保活：首帧否决必须照旧生效。
func TestContinuePendingKeepsHeadVetoWindow(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 5 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })

	// 首段：唯一的帧就是 finish=length 终态（被扣在 tail 里），正文与思考都是 0B ——
	// 客户端一个数据帧都收不到。这是「首段零输出」的空截断救援形态。
	seg0 := `data: {"id":"s0","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":1024,"total_tokens":1034}}` + "\n\n" +
		"data: [DONE]\n\n"
	errSeg := `data: {"error":{"message":"quota exceeded","type":"upstream_error","code":"quota"}}` + "\n\n" + "data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) {
		time.Sleep(120 * time.Millisecond) // 救援段预填充
		return 200, errSeg
	})
	body := `{"model":"glm-5.2","max_tokens":1024,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(seg0)))

	rec := httptest.NewRecorder() // 实现了 http.Flusher，StreamHint 走真流式路径
	_ = upstream.StreamHint(rec, r, nil)
	if len(*reqs) != 1 {
		t.Fatalf("前提失败：应触发一次空截断救援，实际 %d 次", len(*reqs))
	}
	got := rec.Body.String()
	if strings.Contains(got, ": keepalive") {
		t.Fatalf("首帧未交出前不该补保活注释（会把 200 提交掉，救援段首帧报错就换不了号）：%q", got)
	}
	if !strings.HasPrefix(got, "data: ") {
		t.Fatalf("客户端收到的第一个事件应是数据帧（首帧否决窗口内零字节）：%q", got)
	}
}

// trackedBody 记录自己有没有被关掉。Close 可能被多处兜底调用，用 Once 收口。
type trackedBody struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func newTrackedBody(r io.Reader) *trackedBody {
	return &trackedBody{Reader: r, closed: make(chan struct{})}
}

func (b *trackedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// TestContinueCloseReclaimsPendingSegment 换段请求在途时客户端断开（空闲超时、写保活失败，
// StreamHint 随之返回，handler 的 defer 调 Close）：在途请求要被取消，迟到的响应体要被关掉。
// 否则发起协程一直卡在上游直到请求 context 结束；响应若恰好在这时到达，body 连同连接
// 一直没人关。
func TestContinueCloseReclaimsPendingSegment(t *testing.T) {
	old := keepaliveEvery
	keepaliveEvery = 10 * time.Millisecond
	t.Cleanup(func() { keepaliveEvery = old })

	cancelled := make(chan struct{})
	late := newTrackedBody(strings.NewReader(testSeg2Text))
	var once sync.Once
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done() // 上游迟迟不回，直到请求被取消
			once.Do(func() { close(cancelled) })
			// 响应恰好在取消的同时到达：仍交回一个 body，由 Close 负责收走。
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       late,
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	r := newTestContinueReader(t, up, testContinueBody, io.NopCloser(strings.NewReader(testSeg1Text)))
	// 读到第一条保活注释帧 = 换段请求确实在途（等待期没有别的东西可读）。
	// 读放在 goroutine 里：保活没生效时这里要**失败**而不是把整个测试二进制挂死。
	inFlight := make(chan struct{})
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := r.Read(buf)
			if err != nil {
				return
			}
			if strings.Contains(string(buf[:n]), ": keepalive") {
				close(inFlight)
				return
			}
		}
	}()
	select {
	case <-inFlight:
	case <-time.After(3 * time.Second):
		t.Fatal("等不到换段请求在途的保活（换段没走异步或保活没生效）")
	}
	r.Close()
	for _, c := range []struct {
		name string
		ch   chan struct{}
	}{{"在途请求被取消", cancelled}, {"迟到的响应体被关掉", late.closed}} {
		select {
		case <-c.ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("Close 之后%s没有发生", c.name)
		}
	}
}

// TestContinueSegmentFailureFallsThrough 换段请求失败时落到决策链的下一条，与同步发起时
// 一致：首段只有思考就撞了 length，关思考重发失败后仍应走一次续写（同步版 tryStarvedRetry
// 返回 false 后就是这么走的）。
func TestContinueSegmentFailureFallsThrough(t *testing.T) {
	seg0 := `data: {"id":"s0","choices":[{"index":0,"delta":{"reasoning_content":"先想一想"}}]}` + "\n\n" +
		`data: {"id":"s0","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(call int) (int, string) {
		if call == 1 {
			return 500, `{"code":500,"msg":"internal error"}`
		}
		return 200, testSeg2Text
	})
	body := `{"model":"glm-5.2","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`
	r := newTestContinueReader(t, up, body, io.NopCloser(strings.NewReader(seg0)))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 2 {
		t.Fatalf("关思考重发失败后应接着续写一次（共 2 个请求），实际 %d 个", len(*reqs))
	}
	if !strings.Contains(string(out), "继续") {
		t.Fatalf("续写段没接上:\n%s", out)
	}
	if n := strings.Count(string(out), `"finish_reason":`); n != 1 {
		t.Fatalf("终态必须恰好一个，实际 %d 个:\n%s", n, out)
	}
}
