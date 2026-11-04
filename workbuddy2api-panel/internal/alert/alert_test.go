package alert

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeSrc struct{ healthy int }

func (f fakeSrc) Health(realm string) Health {
	if realm == "cn" {
		return Health{Total: 2, Healthy: f.healthy}
	}
	return Health{}
}
func (f fakeSrc) WAFActive() bool { return false }

func TestAlertFiresOnce(t *testing.T) {
	var n int
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		body, _ := io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-WB2A-Signature")
		mac := hmac.New(sha256.New, []byte("s3"))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if gotSig != want {
			t.Errorf("sig %s want %s", gotSig, want)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	m := New(Config{
		Enabled: true, WebhookURL: srv.URL, Secret: "s3",
		StartupGrace: time.Nanosecond, ForTicks: 1, ClearTicks: 1,
		MinHealthyCN: 1, Interval: time.Hour, Timeout: time.Second,
	}, fakeSrc{})
	now := m.startedAt.Add(time.Second)
	m.Tick(now)
	m.Tick(now.Add(time.Second))
	if n != 1 {
		t.Fatalf("posts=%d want 1 (edge trigger, no repeat)", n)
	}
}

// TestNotifyPosts 汇总通知走同一条 webhook 管道：enabled + URL 才发；关闭时静默。
func TestNotifyPosts(t *testing.T) {
	var n int
	var got string
	var sig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		body, _ := io.ReadAll(r.Body)
		got, sig = string(body), r.Header.Get("X-WB2A-Signature")
		w.WriteHeader(204)
	}))
	defer srv.Close()
	m := New(Config{Enabled: true, WebhookURL: srv.URL, Secret: "s3", Interval: time.Hour, Timeout: time.Second}, fakeSrc{})
	m.Notify("growth 队列 第 1 轮 2 项：完成 2 / 跳过 0 / 失败 0")
	if n != 1 || !strings.Contains(got, `"event":"notify"`) || !strings.Contains(got, "growth 队列 第 1 轮") {
		t.Fatalf("posts=%d body=%s want 1 条 event=notify", n, got)
	}
	mac := hmac.New(sha256.New, []byte("s3"))
	mac.Write([]byte(got))
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); sig != want {
		t.Fatalf("sig %s want %s", sig, want)
	}
	New(Config{Interval: time.Hour, Timeout: time.Second}, fakeSrc{}).Notify("x") // 未启用：静默不 panic
	if n != 1 {
		t.Fatalf("posts=%d want 1（未启用不应发）", n)
	}
}
