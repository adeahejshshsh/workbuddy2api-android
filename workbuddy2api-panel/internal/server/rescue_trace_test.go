package server

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// issue #245 补充样本（javycoder）：不是无标点的半句，而是**以句号收尾的进度说明**，
// 正文表示还有没执行的下一步。两条救援都会放过它，但成因完全不同——
// narration 认「尾部是说完的」，toolonly 认「够长 + 句末标点 = 最终答复」。
//
// 线上这两句都只是「没有续写」，无法区分；这条诊断的唯一价值就是把它们分开。
// 只用枚举码，正文/prompt/工具参数一律不进日志（复现者才肯贴）。
func TestRescueTraceRecordsSkipReason(t *testing.T) {
	old := rescueTrace
	rescueTrace = true
	defer func() { rescueTrace = old }()

	text := "已经把 user-store 的批量赋值逐个看完了，三个入口都确认能反序列化外部字段，" +
		"只靠邮箱就能改。这三处的结论先记在这里。"
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"r1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"r1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":106507,"completion_tokens":60,"total_tokens":106567}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, toolLoopBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("这句进度说明不该被救，实际 %d 次", len(*reqs))
	}
	why := strings.Join(r.rescueWhy, " ")
	// narration 更快被挡在「尾部没有承诺词」这一步（它排在尾部形态判定之前）；
	// toolonly 挡在「够长 + 句末标点 = 最终答复」。
	for _, want := range []string{"narration=tail_no_promise", "toolonly=looks_finished"} {
		if !strings.Contains(why, want) {
			t.Errorf("缺少原因码 %q，实际 why=%q", want, why)
		}
	}
}

// 默认关闭：没开开关时一个码都不该记（热路径上不能有额外分配）。
func TestRescueTraceOffByDefault(t *testing.T) {
	old := rescueTrace
	rescueTrace = false
	defer func() { rescueTrace = old }()

	seg := `data: {"id":"r2","choices":[{"index":0,"delta":{"content":"我将开始修改。"}}]}` + "\n\n" +
		`data: {"id":"r2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":301000,"completion_tokens":10,"total_tokens":301010}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, _ := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, chatBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(r.rescueWhy) != 0 {
		t.Errorf("开关未开却记了 %v", r.rescueWhy)
	}
}

// 默认值必须来自环境变量，而不是写死：写死 true 时上面那条会被测试自己强制成 false、
// 静默通过，「诊断默认开」就没人发现。这条直接钉住默认值本身（本仓口径：默认关）。
func TestRescueTraceDefaultFromEnv(t *testing.T) {
	want := os.Getenv("WB2A_RESCUE_TRACE") != "" || os.Getenv("WB2API_RESCUE_TRACE") != ""
	if rescueTrace != want {
		t.Fatalf("rescueTrace=%v，环境变量给出的默认值是 %v（诊断必须默认关闭）", rescueTrace, want)
	}
}

// 今日救援计数（面板概况卡片）：真救一次就 +1，跨天的旧记录不带到今天。
func TestRescueTodayCounter(t *testing.T) {
	rescueToday.mu.Lock()
	rescueToday.day, rescueToday.n = "", nil
	rescueToday.mu.Unlock()

	// 一条真实的承诺句救援（与 TestNarrationPromiseTailStillRescued 同形态）。
	text := strings.Repeat("我把三处改动逐行过了一遍。", 20) + "我将开始修改 internal/server/continue.go"
	raw, _ := json.Marshal(text)
	seg := `data: {"id":"n1","choices":[{"index":0,"delta":{"content":` + string(raw) + `}}]}` + "\n\n" +
		`data: {"id":"n1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":301000,"completion_tokens":500,"total_tokens":301500}}` + "\n\n" +
		"data: [DONE]\n\n"
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segToolOnlyText })
	r := newToolOnlyReader(t, up, chatBody, seg)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("夹具前提不成立：应救一次，实际 %d 次", len(*reqs))
	}
	if n := RescueToday()["narration"]; n != 1 {
		t.Errorf("救了一次，narration 计数应为 1，实际 %d", n)
	}

	// 跨天：把计数日改成昨天，读出来必须是空（也不该把昨天的数算进今天）。
	rescueToday.mu.Lock()
	rescueToday.day = "2000-01-01"
	rescueToday.mu.Unlock()
	if n := RescueToday(); len(n) != 0 {
		t.Errorf("跨天后应归零，实际 %v", n)
	}
}
