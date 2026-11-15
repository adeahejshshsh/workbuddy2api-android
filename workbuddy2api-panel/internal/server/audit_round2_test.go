package server

// audit_round2_test.go 第二轮代码审计（server 核心链路）修复项的回归钉。
//
// 覆盖：
//   - W1A3-02 昵称必须走加锁访问器 NicknameValue()（源码级守卫 + 行为级断言）
//   - W1A3-03 POST /v1/videos 必须纳入 isChatEntry（否则面板完全看不到出视频）
//   - W1A3-04/14 starved 救援后成本台账与 tok 列取两次调用的**合计**
//   - W1A3-05 客户端断连未写响应记 499/interrupted，不再记 500 + WARN
//   - W1A3-09 流水行整行串行写出（注入 MultiWriter 后不交错）

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// TestIsChatEntryCoversVideosResourcePath W1A3-03：OpenAI Videos 资源路径
// POST /v1/videos 注册的是同一个会真扣积分的 h.videos，必须与 /generations、
// /tasks 一视同仁进归档（X-Request-Id / reqlog / 流水行）。
func TestIsChatEntryCoversVideosResourcePath(t *testing.T) {
	for _, p := range []string{
		"/v1/videos", "/v1/videos/generations", "/v1/videos/tasks",
		"/v1/3d/generations", "/v1/3d/tasks",
		"/v1/chat/completions", "/v1/responses", "/v1/messages",
	} {
		if !isChatEntry(p) {
			t.Errorf("isChatEntry(%q)=false，该路径的出站写操作在面板里不可见（W1A3-03）", p)
		}
	}
	for _, p := range []string{"/v1/models", "/status", "/v1/videos/t1", "/"} {
		if isChatEntry(p) {
			t.Errorf("isChatEntry(%q)=true，只读/查询路径不该进请求归档", p)
		}
	}
}

// TestTraceFinalStatusDistinguishesClientCancel W1A3-05：
// 一字节未写就返回时，必须能区分「调用方断开」与「panic/漏写」。
//
// 变异判据：把两个 499 分支去掉（回到「一律 500 + WARN」），本用例红。
func TestTraceFinalStatusDistinguishesClientCancel(t *testing.T) {
	// ① handler 显式置了 interrupted（chatCompletions 的 499 分支）
	tr := &requestTrace{id: "r1", stat: &chatStat{outcome: reqlog.OutcomeInterrupted}}
	if status, warn := traceFinalStatus(0, tr, nil); status != 499 || warn {
		t.Fatalf("interrupted 且未写响应：status=%d warn=%v want 499/false（中断率不该统计成 5xx）", status, warn)
	}
	// ② stat 还没建、但请求上下文已取消（预算闸之前的读体阶段被掐断）
	if status, warn := traceFinalStatus(0, &requestTrace{id: "r2"}, io.ErrClosedPipe); status != 499 || warn {
		t.Fatalf("ctx 已取消且未写响应：status=%d warn=%v want 499/false", status, warn)
	}
	// ③ 真的什么都没发生就返回：仍是 500 + WARN
	if status, warn := traceFinalStatus(0, &requestTrace{id: "r3", stat: &chatStat{}}, nil); status != http.StatusInternalServerError || !warn {
		t.Fatalf("无中断证据且未写响应：status=%d warn=%v want 500/true（panic 或漏写要留痕）", status, warn)
	}
	// ④ 已写出响应：原样记账，不打 WARN
	if status, warn := traceFinalStatus(http.StatusTeapot, &requestTrace{id: "r4"}, nil); status != http.StatusTeapot || warn {
		t.Fatalf("已写响应：status=%d warn=%v want 418/false", status, warn)
	}
}

// TestChatLogRowsAreWrittenWholeUnderConcurrency W1A3-09：
// 流水行必须整行串行写出。main 注入的是 MultiWriter（stdout + 面板环），
// 并发 Fprintf 会在下游拆成多次 Write 而交错成半行。
//
// 判定方式：用一个记录「同时在写」深度的 sink，只要出现过并发进入（overlap），
// 就说明整行写出没有被串行化。
func TestChatLogRowsAreWrittenWholeUnderConcurrency(t *testing.T) {
	var (
		mu       sync.Mutex
		overlap  bool
		inFlight int
		lines    int
	)
	sink := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		inFlight++
		if inFlight > 1 {
			overlap = true
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond) // 放大竞态窗口
		mu.Lock()
		inFlight--
		if strings.HasPrefix(string(p), "| #") {
			lines++
		}
		mu.Unlock()
		return len(p), nil
	})

	oldOut, oldEnabled := chatLogOut, chatLogEnabled
	chatLogOut, chatLogEnabled = sink, true
	t.Cleanup(func() { chatLogOut, chatLogEnabled = oldOut, oldEnabled })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				logChatRow(time.Second, 2*time.Second, "m", "stream", "u1", "nick", 200, 10)
			}
		}()
	}
	wg.Wait()
	if overlap {
		t.Fatal("流水行被并发写出（W1A3-09）：注入 MultiWriter 后会交错成半行")
	}
	if lines != 32 {
		t.Fatalf("落下的流水行 = %d want 32", lines)
	}
}

// writerFunc 把函数适配成 io.Writer。
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestNoBareNicknameReadInChatPath W1A3-02（源码级守卫）：
// handler.go / logging.go 里不允许出现裸的 acct.Nickname —— 面板同步昵称会在
// a.mu 内改写该字段，直读是真实数据竞争（-race 必报）。行为级用例无法稳定复现
// 撕裂，这里直接把「无锁读点」钉死：新增代码若再直读，本用例立刻红。
func TestNoBareNicknameReadInChatPath(t *testing.T) {
	bare := regexp.MustCompile(`\bacct\.Nickname\b`)
	for _, f := range []string{"handler.go", "logging.go"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		if loc := bare.FindStringIndex(string(raw)); loc != nil {
			line := 1 + strings.Count(string(raw[:loc[0]]), "\n")
			t.Fatalf("%s:%d 直读 acct.Nickname（应走 acct.NicknameValue()，见 W1A3-02）", f, line)
		}
	}
}

// TestChatLogCarriesNicknameFromAccessor W1A3-02（行为级）：
// 昵称经加锁访问器取到，且面板改昵称后下一条流水行立刻反映新值。
func TestChatLogCarriesNicknameFromAccessor(t *testing.T) {
	acct := &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "旧昵称"}
	if got := acct.NicknameValue(); got != "旧昵称" {
		t.Fatalf("NicknameValue()=%q want 旧昵称", got)
	}
	// 模拟面板同步昵称（pool.SetNickname 的真实写路径：持 a.Lock() 改字段）。
	acct.Lock()
	acct.Nickname = "新昵称"
	acct.Unlock()
	if got := acct.NicknameValue(); got != "新昵称" {
		t.Fatalf("SetNickname 后 NicknameValue()=%q want 新昵称（访问器必须看到锁内写入）", got)
	}
}

// segStarvedWithCredit 首段：整份额度被思考吃光 + 带 credit（成本台账取数依赖它）。
const segStarvedWithCredit = `data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"想了很久"}}]}` + "\n\n" +
	`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":8192,"total_tokens":8202,"credit":1.5}}` + "\n\n" +
	"data: [DONE]\n\n"

// segRescueWithCredit 救援段：正常收尾 + 同样带 credit。
const segRescueWithCredit = `data: {"id":"c2","choices":[{"index":0,"delta":{"content":"正文"}}]}` + "\n\n" +
	`data: {"id":"c2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":7,"total_tokens":27,"credit":0.5}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestStarvedRescueBooksSummedCreditAndTotal W1A3-04：
// starved 救援成功后，NoteModelCost 必须记 (Σcredit, Σtotal)。
//
// 缺陷现场：credit 取了两次调用之和，total 仍取末次响应 → per-1k 被高估最多 2×；
// 而 costTier 是选号硬过滤（账本是唯一事实来源），后续请求会被错误地压到或避开该号。
//
// 数值：Σcredit=2.0，Σtotal=8229 → per1k ≈ 0.243；只取末次 total 则是 2.0/27*1000 ≈ 74。
func TestStarvedRescueBooksSummedCreditAndTotal(t *testing.T) {
	up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, segRescueWithCredit })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	up.StoreModelMaxOutForTest("cn", map[string]int64{"deepseek-v4.1-flash": 131072})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, string(raw))
		body := segRescueWithCredit
		if len(*reqs) == 1 {
			body = segStarvedWithCredit
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})

	body := `{"model":"deepseek-v4.1-flash","max_tokens":1024,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if len(*reqs) != 2 {
		t.Fatalf("上游调用=%d want 2（首段 + 救援段）", len(*reqs))
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号不在池里")
	}
	var per1k float64
	found := false
	for _, mc := range st.ModelCosts {
		if mc.Model == "deepseek-v4.1-flash" || strings.HasSuffix(mc.Model, "deepseek-v4.1-flash") {
			per1k, found = mc.CostPer1k, true
		}
	}
	if !found {
		t.Fatalf("成本台账没有该模型的观测：%+v", st.ModelCosts)
	}
	want := 2.0 / 8229 * 1000
	if per1k < want*0.9 || per1k > want*1.1 {
		t.Fatalf("per1k=%.4f want≈%.4f（Σcredit/Σtotal；只取末次 total 会得到 ≈74，见 W1A3-04）", per1k, want)
	}
}
