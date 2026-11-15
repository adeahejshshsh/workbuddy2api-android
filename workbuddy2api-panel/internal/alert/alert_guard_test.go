package alert

// alert_guard_test.go 告警通道安全面与并发面的回归用例（审查 W2-C2-08~16）。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestValidateWebhookURL webhook 目标校验：只放行 https（回环可 http）、拒绝私网/元数据
// 地址段（审查 W2-C2-08）。
func TestValidateWebhookURL(t *testing.T) {
	cases := []struct {
		name        string
		url         string
		insecure    bool
		allowPriv   bool
		wantBlocked bool
	}{
		{"回环 http 放行", "http://127.0.0.1:9099/hook", false, false, false},
		{"localhost http 放行", "http://localhost:9099/hook", false, false, false},
		{"公网 http 拒绝（凭据上链路）", "http://hooks.example.com/x", false, false, true},
		{"公网 https 放行", "https://hooks.example.com/x", false, false, false},
		{"云元数据拒绝", "https://169.254.169.254/latest/meta-data/", false, false, true},
		{"云元数据 http 也拒绝", "http://169.254.169.254/latest/meta-data/", true, false, true},
		{"私网 10/8 拒绝", "https://10.1.2.3/hook", false, false, true},
		{"私网 192.168 拒绝", "https://192.168.1.10/hook", false, false, true},
		{"元数据域名拒绝", "https://metadata.google.internal/x", false, false, true},
		{"非 http(s) 协议拒绝", "ftp://hooks.example.com/x", false, false, true},
		{"空 URL 拒绝", "", false, false, true},
		{"显式放行私网", "https://10.1.2.3/hook", false, true, false},
		{"显式放行明文", "http://hooks.example.com/x", true, false, false},
	}
	for _, c := range cases {
		err := validateWebhookURL(c.url, c.insecure, c.allowPriv)
		if got := err != nil; got != c.wantBlocked {
			t.Errorf("%s: blocked=%v want %v (err=%v)", c.name, got, c.wantBlocked, err)
		}
	}
}

// TestNewBlocksDisallowedWebhook 校验失败的 webhook 一律不发，且不影响 Notify 的安全性
// （审查 W2-C2-08）：配置面被污染时不能变成 SSRF 出口。
func TestNewBlocksDisallowedWebhook(t *testing.T) {
	m := New(Config{Enabled: true, WebhookURL: "http://169.254.169.254/latest/meta-data/", Timeout: time.Second}, fakeSrc{healthy: 1})
	if m.blocked == nil {
		t.Fatal("指向云元数据的 webhook 应被拒绝")
	}
	m.Notify("x") // 被拒的通道只记一次日志，不 panic、不发包

	ok := New(Config{Enabled: true, WebhookURL: "http://127.0.0.1:1/hook", Timeout: time.Second}, fakeSrc{healthy: 1})
	if ok.blocked != nil {
		t.Fatalf("回环 http 不该被拒: %v", ok.blocked)
	}
}

// TestSignatureV2CarriesTimestamp 签名要带时间戳（审查 W2-C2-09）：只签 body 的旧头
// 抓到一条包就能无限重放；V2 签 "t.<body>"，接收方校验时间窗即可挡住重放。
// 旧头必须保持不变，既有接收方与脚本不用改。
func TestSignatureV2CarriesTimestamp(t *testing.T) {
	var legacy, v2 string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		legacy = r.Header.Get("X-WB2A-Signature")
		v2 = r.Header.Get("X-WB2A-Signature-V2")
		w.WriteHeader(204)
	}))
	defer srv.Close()
	m := New(Config{Enabled: true, WebhookURL: srv.URL, Secret: "s3", Timeout: time.Second}, fakeSrc{healthy: 1})
	m.Notify("hello")

	mac := hmac.New(sha256.New, []byte("s3"))
	mac.Write(body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); legacy != want {
		t.Fatalf("旧签名头变了（会打断既有接收方）: %s want %s", legacy, want)
	}
	if !strings.HasPrefix(v2, "t=") || !strings.Contains(v2, ",v1=") {
		t.Fatalf("V2 头格式不对: %q", v2)
	}
	parts := strings.SplitN(strings.TrimPrefix(v2, "t="), ",v1=", 2)
	if len(parts) != 2 {
		t.Fatalf("V2 头解析失败: %q", v2)
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("V2 时间戳不是整数: %q", parts[0])
	}
	if d := time.Since(time.Unix(ts, 0)); d > time.Minute || d < -time.Minute {
		t.Fatalf("V2 时间戳偏离当前时刻 %v", d)
	}
	mac2 := hmac.New(sha256.New, []byte("s3"))
	mac2.Write([]byte(parts[0] + "."))
	mac2.Write(body)
	if want := hex.EncodeToString(mac2.Sum(nil)); parts[1] != want {
		t.Fatalf("V2 签名不覆盖时间戳: got %s want %s", parts[1], want)
	}
}

// TestStopIsTerminalRunDoesNotLeak Stop 之后同一 Monitor 不能再起第二个 ticker 协程
// （审查 W2-C2-10）：原实现把 stop 置 nil 后重建 channel，但 stopOnce 已消耗，
// 第二次 Run 起的 goroutine 永远无法 Stop（热重载时泄漏 + 重复告警）。
func TestStopIsTerminalRunDoesNotLeak(t *testing.T) {
	m := New(Config{Enabled: true, WebhookURL: "http://127.0.0.1:1/hook",
		Interval: time.Hour, Timeout: time.Second, StartupGrace: time.Nanosecond, MinHealthyCN: 1},
		fakeSrc{healthy: 5})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !m.running.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	m.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 之后 Run 没有退出")
	}
	m.Stop() // 幂等

	// 再 Run 一次：必须立刻返回（stop 已关闭），而不是起一个没人能停的 ticker。
	done2 := make(chan struct{})
	go func() { m.Run(ctx); close(done2) }()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 之后再次 Run 起了无法停止的循环（goroutine 泄漏）")
	}
}

// TestConcurrentTickFiresOnce 并发 Tick 必须被串行化，且边沿触发只发一次（审查 W2-C2-11）。
func TestConcurrentTickFiresOnce(t *testing.T) {
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	m := New(Config{Enabled: true, WebhookURL: srv.URL, Interval: time.Hour, Timeout: time.Second,
		StartupGrace: time.Nanosecond, ForTicks: 1, ClearTicks: 1, MinHealthyCN: 1}, fakeSrc{healthy: 0})
	now := m.startedAt.Add(time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				m.Tick(now)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&n); got != 1 {
		t.Fatalf("并发 Tick 发了 %d 条告警，want 1（规则状态被打散）", got)
	}
}

// TestNotifySanitizesSecrets Notify 要截断并脱敏（审查 W2-C2-15）：告警群是第二份泄露面。
func TestNotifySanitizesSecrets(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	m := New(Config{Enabled: true, WebhookURL: srv.URL, Timeout: time.Second}, fakeSrc{healthy: 1})
	m.Notify("轮次失败 token=abcdef123456&uid=u1")
	if strings.Contains(body, "abcdef123456") {
		t.Fatalf("通知里的 token 没脱敏: %s", body)
	}
	if !strings.Contains(body, "token=***") {
		t.Fatalf("脱敏占位符不对: %s", body)
	}

	long := strings.Repeat("啊", 4000)
	m.Notify(long)
	if len(body) > notifyMaxBytes+4096 {
		t.Fatalf("超长通知没有截断，body 长度 %d", len(body))
	}
	if !strings.Contains(body, "truncated") {
		t.Fatalf("截断标记缺失，body 长度 %d", len(body))
	}
}

// TestEnabledWithoutWebhookTurnsOff Enabled=true 但没配 URL 时必须显式关掉
// （审查 W2-C2-13）：否则规则状态机会推进到 Firing（事件被消费）而告警永久丢失。
func TestEnabledWithoutWebhookTurnsOff(t *testing.T) {
	m := New(Config{Enabled: true, WebhookURL: "   ", Timeout: time.Second}, fakeSrc{healthy: 0})
	if m.cfg.Enabled {
		t.Fatal("缺 webhook_url 时应把 Enabled 置 false")
	}
	m.Tick(time.Now().Add(time.Hour)) // 不 panic、不发包
}

// TestStartupGracePrefillsState 宽限期内采样但不发；宽限结束时立刻告警，而不是再等
// forTicks 个 tick（审查 W2-C2-16）。
func TestStartupGracePrefillsState(t *testing.T) {
	var n int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&n, 1)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	m := New(Config{Enabled: true, WebhookURL: srv.URL, Interval: time.Hour, Timeout: time.Second,
		StartupGrace: time.Hour, ForTicks: 2, ClearTicks: 1, MinHealthyCN: 1}, fakeSrc{healthy: 0})

	m.Tick(m.startedAt.Add(time.Second)) // 宽限期内：只记状态
	if got := atomic.LoadInt64(&n); got != 0 {
		t.Fatalf("宽限期内不该发告警，发了 %d 条", got)
	}
	m.Tick(m.startedAt.Add(2 * time.Hour)) // 宽限结束后的第一次 Tick
	if got := atomic.LoadInt64(&n); got != 1 {
		t.Fatalf("宽限结束应立即告警一次，实际 %d 条", got)
	}
}

// TestQueueDropsWhenFull 异步队列满时丢弃并计数，不阻塞调用方（审查 W2-C2-12）。
func TestQueueDropsWhenFull(t *testing.T) {
	m := New(Config{Enabled: true, WebhookURL: "http://127.0.0.1:1/hook", Timeout: time.Second}, fakeSrc{healthy: 1})
	m.running.Store(true) // 模拟 Run 已启动（不真正起 sendLoop，避免依赖网络）
	defer m.running.Store(false)
	for i := 0; i < postQueueCap+5; i++ {
		m.Notify("burst")
	}
	if got := len(m.queue); got != postQueueCap {
		t.Fatalf("队列长度 %d want %d", got, postQueueCap)
	}
	if got := m.dropped.Load(); got != 5 {
		t.Fatalf("丢弃计数 %d want 5", got)
	}
}
