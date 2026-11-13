package proxypool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testManager(t *testing.T, path string) *Manager {
	t.Helper()
	m, err := New(path, http.DefaultTransport.(*http.Transport), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.data.Settings.URL = "http://health.invalid/"
	t.Cleanup(m.Close)
	return m
}

func addProxy(t *testing.T, m *Manager, raw string) string {
	t.Helper()
	results, err := m.Import(raw)
	if err != nil || len(results) != 1 || results[0].Status != "added" {
		t.Fatalf("import: %+v, %v", results, err)
	}
	return results[0].ID
}

func probe(t *testing.T, m *Manager, id string) {
	t.Helper()
	if err := m.Probe(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestImportPersistenceAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "proxy-pool.json")
	m := testManager(t, path)
	results, err := m.Import("\nHTTP://user:TOPSECRET@localhost:80/\r\nhttp://user:TOPSECRET@LOCALHOST\nbad://user:TOPSECRET@host\nsocks5h://[::1]:1080\n")
	if err != nil || len(results) != 4 || results[0].Line != 2 || results[1].Status != "duplicate" || results[2].Status != "invalid" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	id := results[0].ID
	if err := m.Bind("account", Binding{Mode: "fixed", ProxyID: id}); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(id, Input{Name: "Office", URL: "http://user@localhost:8080"}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(m.Snapshot())
	if strings.Contains(string(snapshot), "TOPSECRET") || !strings.Contains(string(snapshot), `"password_set":true`) {
		t.Fatalf("snapshot leaked password: %s", snapshot)
	}
	stored, _ := os.ReadFile(path)
	if !strings.Contains(string(stored), "TOPSECRET") {
		t.Fatal("blank edit lost stored password")
	}
	reloaded := testManager(t, path)
	s := reloaded.Snapshot()
	if len(s.Proxies) != 2 || s.Proxies[0].Name != "Office" || s.Proxies[0].Status != "pending" || s.Bindings["account"].ProxyID != id {
		t.Fatalf("reload=%+v", s)
	}
	if err := m.Delete(id); !errors.Is(err, ErrBound) {
		t.Fatalf("bound deletion=%v", err)
	}
	if err := m.Unbind("account"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidURLsDoNotEchoCredentials(t *testing.T) {
	for _, raw := range []string{
		"http://HOST:0", "http://HOST:65536", "http://HOST/path", "http://HOST?x=secret",
		"http://HOST#fragment", "ftp://user:TOPSECRET@HOST", "http://user:%zz@HOST", "http://:TOPSECRET@HOST", "HOST:8080",
	} {
		_, err := parseURL(raw)
		if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
			t.Fatalf("%q: %v", raw, err)
		}
	}
}

func TestWriteFailureDoesNotPublish(t *testing.T) {
	m := testManager(t, "")
	id := addProxy(t, m, "http://localhost:8080")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.path = filepath.Join(blocker, "pool.json")
	if err := m.Update(id, Input{Name: "Changed"}); err == nil {
		t.Fatal("expected storage error")
	}
	if m.Snapshot().Proxies[0].Name == "Changed" {
		t.Fatal("failed save changed runtime")
	}
	if err := m.Bind("u1", Binding{Mode: "direct"}); err == nil || len(m.Snapshot().Bindings) != 0 {
		t.Fatal("failed binding save changed runtime")
	}
}

func TestHealthThresholdsAndManualDisable(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("probe carried account credentials")
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()
	m := testManager(t, "")
	id := addProxy(t, m, server.URL)
	probe(t, m, id)
	if m.Snapshot().Proxies[0].Status != "healthy" {
		t.Fatal("initial success did not enable")
	}
	status.Store(502)
	for range 2 {
		probe(t, m, id)
	}
	if m.Snapshot().Proxies[0].Status != "healthy" {
		t.Fatal("disabled before failure threshold")
	}
	probe(t, m, id)
	if m.Snapshot().Proxies[0].Status != "unhealthy" {
		t.Fatal("failure threshold not applied")
	}
	status.Store(200)
	probe(t, m, id)
	if m.Snapshot().Proxies[0].Status != "unhealthy" {
		t.Fatal("recovered before success threshold")
	}
	probe(t, m, id)
	if m.Snapshot().Proxies[0].Status != "healthy" {
		t.Fatal("did not recover")
	}
	off := false
	if err := m.Update(id, Input{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		probe(t, m, id)
	}
	if m.Snapshot().Proxies[0].Status != "disabled" {
		t.Fatal("probe overrode manual disable")
	}
}

func TestStaleProbeDiscarded(t *testing.T) {
	for _, mutation := range []string{"edit", "delete", "settings"} {
		t.Run(mutation, func(t *testing.T) {
			started, finish := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-finish
				w.WriteHeader(200)
			}))
			defer server.Close()
			m := testManager(t, "")
			id := addProxy(t, m, server.URL)
			done := make(chan struct{})
			go func() { _ = m.Probe(context.Background(), id); close(done) }()
			<-started
			var err error
			switch mutation {
			case "edit":
				off := false
				err = m.Update(id, Input{Enabled: &off})
			case "delete":
				err = m.Delete(id)
			case "settings":
				s := DefaultSettings()
				s.FailureThreshold = 2
				err = m.Configure(s)
			}
			close(finish)
			<-done
			if err != nil {
				t.Fatal(err)
			}
			snapshot := m.Snapshot()
			if len(snapshot.Proxies) > 0 && !snapshot.Proxies[0].CheckedAt.IsZero() {
				t.Fatal("stale probe published")
			}
		})
	}
}

func TestRoutingRandomFixedDirectAndRedirect(t *testing.T) {
	m := testManager(t, "")
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "direct") }))
	defer direct.Close()
	request := func(uid string) (string, error) {
		req, _ := http.NewRequest("GET", direct.URL, nil)
		resp, err := m.Do(&http.Client{Timeout: time.Second}, req, uid, "cn")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}
	if body, err := request("u"); err != nil || body != "direct" {
		t.Fatalf("empty pool: %q %v", body, err)
	}
	var ids []string
	for i := range 2 {
		label := fmt.Sprint(i)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirect" {
				if r.URL.Query().Get("via") != "" && r.URL.Query().Get("via") != label {
					t.Error("redirect changed proxy")
				}
				http.Redirect(w, r, "http://destination.invalid/final?via="+label, 302)
				return
			}
			if via := r.URL.Query().Get("via"); via != "" && via != label {
				t.Error("redirect changed proxy")
			}
			_, _ = io.WriteString(w, label)
		}))
		defer srv.Close()
		ids = append(ids, addProxy(t, m, srv.URL))
	}
	if _, err := request("u"); !IsError(err) {
		t.Fatal("pending pool silently used direct")
	}
	for _, id := range ids {
		probe(t, m, id)
	}
	seen := map[string]bool{}
	for range 100 {
		body, err := request("u")
		if err != nil {
			t.Fatal(err)
		}
		seen[body] = true
	}
	if len(seen) != 2 {
		t.Fatalf("random routes=%v", seen)
	}
	if err := m.Bind("fixed", Binding{Mode: "fixed", ProxyID: ids[0]}); err != nil {
		t.Fatal(err)
	}
	if body, err := request("fixed"); err != nil || body != "0" {
		t.Fatalf("fixed: %q %v", body, err)
	}
	for range 10 {
		req, _ := http.NewRequest("GET", "http://destination.invalid/redirect", nil)
		resp, err := m.Do(&http.Client{Timeout: time.Second}, req, "u", "cn")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	off := false
	if err := m.Update(ids[0], Input{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := request("fixed"); !IsError(err) {
		t.Fatal("fixed binding failed open")
	}
	if err := m.Bind("direct", Binding{Mode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if body, err := request("direct"); err != nil || body != "direct" {
		t.Fatalf("explicit direct: %q %v", body, err)
	}
	if err := m.Update(ids[1], Input{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	// 全部停用 = 代理池关掉，随机绑定回落直连（issue #150）；fixed 仍须报错——
	// 显式钉住的代理不可用时静默直连会把本机 IP 泄漏给上游（上面那条已验）。
	if body, err := request("u"); err != nil || body != "direct" {
		t.Fatalf("all proxies disabled should fall back to direct: %q %v", body, err)
	}
}

func TestProbeConcurrencyBound(t *testing.T) {
	var active, peak atomic.Int32
	entered, release := make(chan struct{}, 32), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		// 非阻塞：两次 CheckAll × 24 个节点会产生 48 个请求，但下方只消费 8 个就离开，
		// 缓冲满了再阻塞发送会让 handler 永远卡住、wg.Wait 挂到 go test 超时（CI 实测）。
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(200)
	}))
	defer server.Close()
	m := testManager(t, "")
	for i := range 24 {
		addProxy(t, m, strings.Replace(server.URL, "://", fmt.Sprintf("://user%d:pass@", i), 1))
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); m.CheckAll(context.Background()) }()
	}
	for range 8 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("workers did not start")
		}
	}
	close(release)
	wg.Wait()
	if peak.Load() > 8 {
		t.Fatalf("concurrent probes=%d", peak.Load())
	}
}

func TestPeriodicRunRecoversAndStops(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	m := testManager(t, "")
	id := addProxy(t, m, server.URL)
	m.mu.Lock()
	m.health[id] = health{status: "unhealthy"}
	m.mu.Unlock()
	s := DefaultSettings()
	s.URL = "http://health.invalid/"
	s.IntervalSeconds, s.SuccessThreshold = 1, 1
	if err := m.Configure(s); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.health[id] = health{status: "unhealthy"}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(3 * time.Second)
	for m.Snapshot().Proxies[0].Status != "healthy" {
		select {
		case <-deadline:
			t.Fatal("periodic probe did not recover disabled node")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestProxyResponseErrorsRetainIdentityAndCancellation(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "truncated")
	}))
	defer proxy.Close()
	m := testManager(t, "")
	id := addProxy(t, m, proxy.URL)
	probe(t, m, id)
	req, _ := http.NewRequest("GET", "http://origin.invalid/truncated", nil)
	resp, err := m.Do(http.DefaultClient, req, "u1", "cn")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !IsError(err) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body read error lost proxy identity or cause: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", "http://origin.invalid/", nil)
	_, err = m.Do(http.DefaultClient, req, "u1", "cn")
	if !IsError(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost proxy identity or cause: %v", err)
	}
}
