package server

// 端点审计（CODE-AUDIT-FULL §10.2 / §10.3）回归用例。
//
// 这一批修复的共同点：全是「静默行为」——截断不报错、名额不释放、记账被丢掉、
// 响应头原样回写。静默行为没有用例钉住就等于没修：下次重构把 io.LimitReader 写回去，
// 测试照样全绿。所以每条用例都按「修复前会怎么红」来写断言。
//
// 编号对应报告条目：W1A4-xx（§10.2）、W3C8-xx（§10.3）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// —— 静默截断 → 413（W1A4-20 / W3C8-07 / W3C8-24 / W3C8-32）——

// search 的 1MiB 上限：超限必须是 413，而不是「半截 body → invalid json → 400」。
// 修复前：io.LimitReader 截断后 json.Unmarshal 报 unexpected end of JSON input，
// 客户端拿到 400 且完全看不出是「太大」，只会把同一个大 body 反复重发。
func TestSearchBodyOverLimitReturns413(t *testing.T) {
	f := &searchFake{status: 200, body: searchOKBody}
	h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()})

	body := `{"query":"` + strings.Repeat("x", (1<<20)+1024) + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/search", strings.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413（超限不能被谎报成 400 invalid json）body=%.200s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "request_too_large") {
		t.Fatalf("错误码应是 request_too_large：%.200s", rec.Body)
	}
	if len(f.paths) != 0 {
		t.Fatalf("超限请求不该打到上游，实际 %d 次", len(f.paths))
	}
}

// fetch 同一处：1MiB 上限、超限 413。
func TestFetchBodyOverLimitReturns413(t *testing.T) {
	f := &searchFake{status: 200, body: `{"content":"x"}`}
	h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()})

	body := `{"url":"https://example.com","pad":"` + strings.Repeat("y", (1<<20)+1024) + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 body=%.200s", rec.Code, rec.Body)
	}
	if len(f.paths) != 0 {
		t.Fatalf("超限请求不该打到上游，实际 %d 次", len(f.paths))
	}
}

// 视频提交（4MiB）与任务查询（64KiB）此前都用 io.LimitReader 静默截断。
func TestVideoEndpointsBodyOverLimitReturns413(t *testing.T) {
	t.Run("generations", func(t *testing.T) {
		f := &videoFake{on: func(videoCall) (int, string) { return 200, videoTaskRunning }}
		h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()})
		body := `{"prompt":"` + strings.Repeat("z", videoMaxBody+1024) + `"}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/videos/generations", strings.NewReader(body)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("code=%d want 413 body=%.200s", rec.Code, rec.Body)
		}
		if n := f.count("/v2/videos/generations"); n != 0 {
			t.Fatalf("超限提交不该打上游，实际 %d 次", n)
		}
	})
	t.Run("tasks", func(t *testing.T) {
		f := &videoFake{on: func(videoCall) (int, string) { return 200, videoTaskRunning }}
		h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()})
		body := `{"task_id":"` + strings.Repeat("q", (64<<10)+64) + `"}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/videos/tasks", strings.NewReader(body)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("code=%d want 413 body=%.200s", rec.Code, rec.Body)
		}
		if n := f.count("/v2/videos/tasks"); n != 0 {
			t.Fatalf("超限请求不该打上游，实际 %d 次", n)
		}
	})
}

// —— /v1/fetch 的 SSRF 面（W1A4-10 / W3C8-29）——

// 内网 / 回环 / 云元数据 / 非 80-443 端口：网关侧就拒，且**一次上游都不打**。
// 修复前：只校验 scheme，`{"url":"http://169.254.169.254/..."}` 会被原样交给上游代取。
func TestFetchRejectsPrivateTargets(t *testing.T) {
	targets := []string{
		`http://127.0.0.1/`,
		`http://127.0.0.1:8080/admin`,
		`http://169.254.169.254/latest/meta-data/iam/security-credentials/`,
		`http://10.1.2.3/`,
		`http://172.16.0.1/`,
		`http://192.168.1.1/`,
		`http://[::1]/`,
		`http://localhost/`,
		`http://metadata.google.internal/computeMetadata/v1/`,
		`https://example.com:6379/`,
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			f := &searchFake{status: 200, body: `{"content":"x"}`}
			h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()})
			body, _ := json.Marshal(map[string]string{"url": target})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(string(body))))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400（内网/元数据地址必须在网关侧拒掉）body=%.200s", rec.Code, rec.Body)
			}
			if len(f.paths) != 0 {
				t.Fatalf("被拒的目标不该打到上游，实际 %d 次", len(f.paths))
			}
		})
	}
	// 公网目标照旧放行（别把收口做成误伤）。
	f := &searchFake{status: 200, body: `{"url":"https://example.com/a","content":"正文"}`}
	h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch", strings.NewReader(`{"url":"https://example.com/a"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("公网目标应放行，code=%d body=%s", rec.Code, rec.Body)
	}
}

// realm=global 但部署没开 global 域：直接 400，不打上游（W3C8-32 / W3C8-28）。
func TestFetchAndEmbeddingsRealmGlobalDisabled(t *testing.T) {
	t.Run("fetch", func(t *testing.T) {
		f := &searchFake{status: 200, body: `{"content":"x"}`}
		h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()}) // GlobalEnabled 缺省 false
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
			strings.NewReader(`{"url":"https://example.com/a","realm":"global"}`)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
		}
		if len(f.paths) != 0 {
			t.Fatalf("realm 被拒时不该打上游，实际 %d 次", len(f.paths))
		}
	})
	t.Run("embeddings", func(t *testing.T) {
		f := &searchFake{status: 200, body: `{"data":[{"embedding":[0.1]}]}`}
		h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client(), EmbeddingsEnabled: true})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/embeddings",
			strings.NewReader(`{"model":"bge-m3","input":"x","realm":"global"}`)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
		}
		if len(f.paths) != 0 {
			t.Fatalf("realm 被拒时不该打上游，实际 %d 次", len(f.paths))
		}
	})
}

// 15021 是**域名级**拒绝：不该换号（W3C8-31）。
// 修复前：continue 让下一轮 PickAcquire 换另一个账号再抓同一个域名，3 个号全白烧。
func TestFetchDomainRejectedDoesNotRotateAccounts(t *testing.T) {
	f := &searchFake{status: 403, body: `{"code":15021,"msg":"domain not allowed by routing policy"}`}
	h := NewHandler(Config{Pool: probePool(3, ""), Upstream: f.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/fetch",
		strings.NewReader(`{"url":"https://docs.google.com/x"}`)))

	if n := len(f.paths); n != 1 {
		t.Fatalf("域名级 15021 只该试 1 个账号，实际打了 %d 次（3 个号白烧）", n)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403 body=%s", rec.Code, rec.Body)
	}
}

// fetch 的在途名额必须用 defer 释放（W3C8-30）：上游 panic 时普通语句会被跳过，
// 该账号的在途计数永久 +1，累积到上限就再也选不出来（「幽灵在途」逐个吃掉可用账号）。
// 这里让 transport 直接 panic（upstream 的 WebFetch 不 recover，只有 refresh/观察者有），
// 再断言名额已归还。
func TestFetchReleasesInFlightSlotOnPanic(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetMaxInFlight(1)
	up := &upstream.Client{
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			panic("transport boom")
		})},
	}
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/fetch", nil)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("预期上游 panic 穿透（否则这条用例测不到 defer 释放）")
			}
		}()
		_, _ = h.fetchInRealm(rec, r, "https://example.com/a", "cn", map[string]bool{})
	}()
	if st, _ := p.Status("c1"); st.InFlight != 0 {
		t.Fatalf("panic 后在途名额没归还（幽灵在途）：%d", st.InFlight)
	}
}

// 探测拿到 200 + 空 body 时不能当成「命中」收养（W3C8-55）：空状态会被 noteState 的
// 「st.Status == "" 早退」拦住，任务永远停在 queued，还白占台账容量。
func TestVideoProbeIgnoresEmptyState(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) { return 200, "" }}
	h := NewHandler(Config{Pool: probePool(2, ""), Upstream: f.client()})
	rec := getVideo(t, h, "empty-state")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404（空回执不算命中）body=%s", rec.Code, rec.Body)
	}
	if _, ok := h.videoLedger.get("empty-state"); ok {
		t.Fatal("空回执不该被收养进台账")
	}
}

// —— 台账写路径（W3C8-01 / W3C8-03）——

// put 不得把已记账条目的 Charged 清回 false（否则同一笔用量被记两次）。
func TestLedgerPutKeepsChargedOnResubmit(t *testing.T) {
	v := newVideoTaskStore()
	v.put("t1", "u1", "cn", "minimax-video-h3")
	if !v.claimCharge("t1") {
		t.Fatal("首次应赢得记账权")
	}
	before, _ := v.get("t1")
	time.Sleep(2 * time.Millisecond)
	v.put("t1", "u2", "global", "kling-video-v3") // 上游幂等回执 / 客户端重试
	after, ok := v.get("t1")
	if !ok || !after.Charged {
		t.Fatalf("重提交把已记账标记清掉了（会重复扣费）：%+v", after)
	}
	if v.claimCharge("t1") {
		t.Fatal("重提交后不该重新赢得记账权")
	}
	if !after.Created.Equal(before.Created) {
		t.Errorf("重提交刷新了 Created（TTL 被续命）：%v → %v", before.Created, after.Created)
	}

	d := newThreeDTaskStore()
	d.put("j1", "u1", "cn", "3.1", upstream.ThreeDState{Status: "DONE", Credit: 20})
	if !d.claimCharge("j1") {
		t.Fatal("3D 首次应赢得记账权")
	}
	d.put("j1", "u1", "cn", "3.1", upstream.ThreeDState{Status: "DONE", Credit: 20})
	if d.claimCharge("j1") {
		t.Fatal("3D 重提交后不该重新赢得记账权（重复扣费）")
	}
}

// adopt 必须走容量淘汰（W3C8-01）：adopt 由「台账里没有的 task_id」触发，id 由客户端控制，
// 不 GC 就是一条无界增长的内存写入通道。
func TestLedgerAdoptRespectsCapacity(t *testing.T) {
	v := newVideoTaskStore()
	base := time.Now().Add(-time.Hour)
	v.mu.Lock()
	for i := 0; i < videoTaskMax; i++ {
		v.m[fmtID("v", i)] = &videoTaskOwner{UID: "u1", Created: base.Add(time.Duration(i) * time.Second)}
	}
	v.mu.Unlock()
	v.adopt("adopted-video", "u1", "cn", "m", false)
	v.mu.Lock()
	nv := len(v.m)
	_, adoptedKept := v.m["adopted-video"]
	v.mu.Unlock()
	if nv >= videoTaskMax {
		t.Fatalf("adopt 没触发 GC：台账 %d 条（上限 %d）", nv, videoTaskMax)
	}
	if !adoptedKept {
		t.Fatal("刚收养的条目不该被自己触发的那次 GC 淘汰")
	}

	d := newThreeDTaskStore()
	d.mu.Lock()
	for i := 0; i < threeDTaskMax; i++ {
		d.m[fmtID("j", i)] = &threeDTaskOwner{UID: "u1", Created: base.Add(time.Duration(i) * time.Second)}
	}
	d.mu.Unlock()
	d.adopt("adopted-3d", "u1", "cn", "3.1", false)
	d.mu.Lock()
	nd := len(d.m)
	d.mu.Unlock()
	if nd >= threeDTaskMax {
		t.Fatalf("3D adopt 没触发 GC：台账 %d 条（上限 %d）", nd, threeDTaskMax)
	}
}

func fmtID(prefix string, i int) string { return fmt.Sprintf("%s-%04d", prefix, i) }

// —— 等待方必须可取消（W1A4-05 / W3C8-04）——

// 修复前等待方是裸 `<-f.done`：发起方的上游调用不返回，等待方就一直挂着（客户端断开也不释放）。
func TestVideoPollSharedWaitHonorsContext(t *testing.T) {
	s := newVideoTaskStore()
	// 手工造一个「发起方还在跑」的在途 flight：done 不关闭。
	f := &videoFlight{done: make(chan struct{})}
	s.flights["hot-task"] = f
	t.Cleanup(func() {
		close(f.done)
		s.mu.Lock()
		delete(s.flights, "hot-task")
		s.mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, _, err := s.pollShared(ctx, "hot-task", func() (upstream.VideoTaskState, string, string, error) {
		t.Error("等待方不该自己发起上游调用")
		return upstream.VideoTaskState{}, "", "", nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("等待方应按 ctx 返回 DeadlineExceeded，got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("取消后应立刻返回，实际等了 %s", d)
	}
}

// —— 计费口径（W1A4-07 / W3C8-53 / W1A4-18）——

// 收养任务的 model 为空时会被 NoteModelCost 直接丢弃 → 积分永久不入账。
// 修复后：adopt 落 unknown-adopted 占位，账记上；回执侧仍不编造 model 字段。
func TestVideoAdoptedTaskIsBilledUnderSentinelModel(t *testing.T) {
	p := probePool(1, "")
	h := NewHandler(Config{Pool: p, Upstream: (&videoFake{on: func(videoCall) (int, string) { return 200, videoTaskRunning }}).client()})

	h.videoLedger.adoptFor("adopted-task", "a01", "cn", "", false, "")
	o, ok := h.videoLedger.get("adopted-task")
	if !ok || o.Model != videoModelAdopted {
		t.Fatalf("收养条目应落占位模型 %q：%+v ok=%v", videoModelAdopted, o, ok)
	}
	if got := displayModel(o.Model, "cn"); got != "" {
		t.Fatalf("收养任务不该在回执里编造 model：%q", got)
	}

	st := &chatStat{start: time.Now(), model: o.Model, mode: "video", toks: -1}
	h.recordVideoUsage(st, "cn", "a01", o.Model, "",
		"adopted-task", upstream.VideoTaskState{Status: "completed", Credit: 57.14, Duration: 4}, time.Now())

	acc, _ := p.Status("a01")
	found := false
	for _, mc := range acc.ModelCosts {
		if mc.Model == videoModelAdopted {
			found = true
		}
	}
	if !found {
		t.Fatalf("收养任务的积分没入账（修复前 model=\"\" 被 NoteModelCost 直接丢掉）：%+v", acc.ModelCosts)
	}
	if acc.Credits >= 1000 {
		t.Fatalf("余额没下账：credits=%d（want < 1000）", acc.Credits)
	}
}

// 3D 收养同样不能编造版本号（W1A4-18）：此前 model 为空一律回 "3.1"。
func TestThreeDAdoptedJobOmitsFabricatedModel(t *testing.T) {
	h := NewHandler(Config{ThreeDEnabled: true, Upstream: upstream.New()})
	h.threeDLedger.adopt("job-adopted", "u1", "cn", "", true)
	o, ok := h.threeDLedger.get("job-adopted")
	if !ok || o.Model != threeDModelAdopted {
		t.Fatalf("收养条目应落占位模型 %q：%+v ok=%v", threeDModelAdopted, o, ok)
	}
	out := h.threeDPayload("job-adopted", upstream.ThreeDState{JobID: "job-adopted", Status: "DONE"}, o.Model)
	if v, has := out["model"]; has {
		t.Fatalf("模型未知时不该给 model 字段（此前编造成 3.1），got %v", v)
	}
	// 已知模型仍要原样透出（别把修复做成丢字段）。
	out = h.threeDPayload("job-known", upstream.ThreeDState{JobID: "job-known", Status: "DONE"}, "3.0")
	if out["model"] != "3.0" {
		t.Fatalf("已知模型应原样回显，got %v", out["model"])
	}
}

// —— /content 的响应头收口（W3C8-06）——

// 上游 Content-Type 原样回写 = 网关域下的同源 XSS 载体（text/html 会在本域执行）。
func TestVideoContentHardensContentType(t *testing.T) {
	allowMediaLoopback(t)
	payload := []byte("\x00\x00\x00\x18ftypmp42fake-bytes")

	newHandler := func(ct string) *Handler {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write(payload)
		}))
		t.Cleanup(srv.Close)
		h := NewHandler(Config{Pool: probePool(1, ""), Upstream: (&videoFake{on: func(videoCall) (int, string) { return 200, videoTaskRunning }}).client()})
		h.videoLedger.put("task-ct", "a01", "cn", "minimax-video-h3")
		h.videoLedger.noteState("task-ct", upstream.VideoTaskState{Status: "completed", URLs: []string{srv.URL + "/v.mp4"}})
		return h
	}

	// 视频类型：保持原样（既有客户端依赖它），但必须补 nosniff + attachment。
	h := newHandler("video/mp4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/videos/task-ct/content", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("video/mp4 应原样保留，got %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("缺 X-Content-Type-Options: nosniff")
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("缺 Content-Disposition: attachment，got %q", cd)
	}

	// 非视频类型：一律降级成 application/octet-stream（不在本域当 HTML/SVG 渲染）。
	h = newHandler("text/html; charset=utf-8")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/videos/task-ct/content", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("text/html 必须降级成 application/octet-stream，got %q", ct)
	}
}

// —— 日志/错误文本的编码健壮性（W3C8-09）——

func TestClipTextKeepsValidUTF8(t *testing.T) {
	const s = "中文中文"
	got := clipText(s, 4) // 4 字节落在「文」的中间
	if !utf8.ValidString(got) {
		t.Fatalf("截断产出了非法 UTF-8：%q (% x)", got, []byte(got))
	}
	if got != "中…" {
		t.Fatalf("应按 rune 边界回退到「中…」，got %q", got)
	}
	if clipText("abcdef", 3) != "abc…" {
		t.Fatalf("纯 ASCII 截断行为不该变：%q", clipText("abcdef", 3))
	}
	if clipText("abc", 10) != "abc" {
		t.Fatalf("未超长时应原样返回：%q", clipText("abc", 10))
	}
}

// —— embeddings 的 index 对齐（W1A4-19 / W3C8-26）——

// 上游乱序返回时必须按 index 落位；修复前 out[i]=it 会让客户端按数组顺序取到别人的向量。
func TestEmbeddingsReordersByIndex(t *testing.T) {
	const upstreamBody = `{"code":0,"msg":"ok","model":"bge-m3","data":[` +
		`{"embedding":[0.3,0.4],"index":1},{"embedding":[0.1,0.2],"index":0}]}`
	f := &searchFake{status: 200, body: upstreamBody}
	h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client(), EmbeddingsEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"bge-m3","input":["a","b"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(got.Data) != 2 {
		t.Fatalf("data 长度=%d want 2", len(got.Data))
	}
	if got.Data[0].Index != 0 || got.Data[0].Embedding[0] != 0.1 {
		t.Fatalf("data[0] 应是 index=0 的向量 [0.1,0.2]，got %+v", got.Data[0])
	}
	if got.Data[1].Index != 1 || got.Data[1].Embedding[0] != 0.3 {
		t.Fatalf("data[1] 应是 index=1 的向量 [0.3,0.4]，got %+v", got.Data[1])
	}
}

// —— 探测负缓存的淘汰策略（W1A4-21）——

// 修复前负缓存满了是「map 迭代序随手丢一条」（随机）：垃圾 id 有机会挤掉正常 id 的
// 静默期记录，让正常用户的未知任务查询被反复真探测，把 30/min 全局额度耗在别人身上。
func TestProbeMissEvictsEarliestExpiry(t *testing.T) {
	p := newTaskProber(probeConfig{max: 5, budget: 30, retry: time.Minute, keep: time.Hour, pause: time.Minute, missed: 8})
	now := time.Now()
	p.mu.Lock()
	for i := 0; i < p.cfg.missed; i++ {
		// victim 最早过期；其余依次更晚。
		p.miss[fmtID("id", i)] = &probeMiss{expire: now.Add(time.Duration(i+1) * time.Minute)}
	}
	p.mu.Unlock()

	p.missed("fresh-id", 1, now)

	p.mu.Lock()
	_, victimAlive := p.miss["id-0"]
	_, freshAlive := p.miss["fresh-id"]
	n := len(p.miss)
	p.mu.Unlock()
	if victimAlive {
		t.Error("应淘汰最早过期的那条（id-0），而不是随机一条")
	}
	if !freshAlive {
		t.Error("新记录必须留下")
	}
	if n > p.cfg.missed {
		t.Errorf("负缓存超过硬上限：%d > %d", n, p.cfg.missed)
	}
}

// —— 上传图片的 MIME 收口（W3C8-13）与 multipart 解析（W1A4-27）——

// 非位图 Content-Type（text/html / image/svg+xml）此前被原样拼进 data:<ct>;base64,
// 交给上游；现在直接拒。
func TestImageUploadRejectsNonImageMime(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "cn:hunyuan-image-alpha")
	_ = mw.WriteField("prompt", "改成蓝色")
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="image"; filename="x.html"`}
	hdr["Content-Type"] = []string{"text/html"}
	fw, _ := mw.CreatePart(hdr)
	_, _ = fw.Write([]byte("<script>alert(1)</script>"))
	_ = mw.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（非位图上传应被拒）body=%.200s", rec.Code, rec.Body)
	}
	if len(f.paths) != 0 {
		t.Fatalf("被拒的上传不该打上游，实际 %d 次", len(f.paths))
	}
}

// 未声明类型（Go 的 CreateFormFile 就是 octet-stream）：不能误伤，按 magic bytes 判，
// 判不出位图按 png 兜底（此前是把 octet-stream 原样塞进 data URL）。
func TestImageUploadNormalizesOctetStream(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "cn:hunyuan-image-alpha")
	_ = mw.WriteField("prompt", "改成蓝色")
	fw, _ := mw.CreateFormFile("image", "in.png") // Content-Type: application/octet-stream
	_, _ = fw.Write([]byte("PNGDATA"))
	_ = mw.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/images/edits", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%.200s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	imgs, _ := sent["image"].([]any)
	if len(imgs) != 1 {
		t.Fatalf("image 应归一成 1 个元素：%v", sent["image"])
	}
	if !strings.HasPrefix(imgs[0].(string), "data:image/") {
		t.Fatalf("octet-stream 应归一成 image/*，got %q", imgs[0].(string))
	}
}

// —— 工具侧出图的预算闸（W3C8-16）——

// imageGenerate 是托管 image_generation 工具的执行路径，此前**不判闸**：
// 预算打穿后 HTTP 入口 429，工具侧仍可继续出图扣费。
func TestImageGenerateBlockedByBudgetGate(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})
	h.SetBudgetLimit(1)
	h.budget.add(5, true)

	if _, err := h.imageGenerate(context.Background(), map[string]any{"prompt": "一只猫"}); err == nil {
		t.Fatal("预算打穿后 imageGenerate 应返回错误（此前会照常出图扣费）")
	}
	if len(f.paths) != 0 {
		t.Fatalf("被闸拒绝后不该打上游，实际 %d 次", len(f.paths))
	}
}

// —— 嵌入端点必须占在途名额（W1A4-03）——

// images / videos / 3D 在 #152 之后都走 PickAcquireExcludingForRealm，只有 embeddings
// 还在用不占位的 PickExcludingForRealm：max_in_flight 对嵌入端点完全失效。
// 这里在上游调用**期间**读账号的在途数：修复前恒 0，修复后应为 1，且请求结束归 0。
func TestEmbeddingsHoldsAndReleasesInFlightSlot(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetMaxInFlight(1)
	var during int32
	up := &upstream.Client{
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if st, ok := p.Status("c1"); ok {
				atomic.StoreInt32(&during, int32(st.InFlight))
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"embedding":[0.1]}],"usage":{"total_tokens":3}}`)),
			}, nil
		})},
	}
	h := NewHandler(Config{Pool: p, Upstream: up, EmbeddingsEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"bge-m3","input":"x"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&during); got != 1 {
		t.Fatalf("上游调用期间应占住在途名额（max_in_flight=1），实际 %d（修复前恒 0）", got)
	}
	if st, _ := p.Status("c1"); st.InFlight != 0 {
		t.Fatalf("请求结束后在途名额没释放：%d", st.InFlight)
	}
}

// —— 视频同步等待不得无视限流冷却（W3C8-05）——

// 撞过 14003 之后有 30s 冷却窗口；修复前 awaitVideo 每 5s 继续打同一个账号，
// 把冷却续上、最后返回 202 让用户以为任务丢了。这里只验证「冷却期内不再打上游」。
func TestAwaitVideoHonorsRateLimitCooldown(t *testing.T) {
	f := &videoFake{on: func(videoCall) (int, string) { return 200, videoTaskRunning }}
	h := NewHandler(Config{Pool: probePool(1, ""), Upstream: f.client()})
	const id = "task-cooldown"
	h.videoLedger.put(id, "a01", "cn", "minimax-video-h3")
	h.videoLedger.noteLimited(id, 150*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a := h.cfg.Pool.AuthByUID("a01")
	if a == nil {
		t.Fatal("测试账号应在池里")
	}
	if _, err := h.awaitVideo(ctx, a, id, 120*time.Millisecond); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("冷却期内等待不该报错：%v", err)
	}
	if n := f.count("/v2/videos/tasks"); n != 0 {
		t.Fatalf("冷却期内不该打上游，实际 %d 次", n)
	}
}
