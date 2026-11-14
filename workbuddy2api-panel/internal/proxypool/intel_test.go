package proxypool

import (
	"sync/atomic"
	"strings"
	"net/http/httptest"
	"net/http"
	"testing"
	"time"
)

// TestClassifyExit 钉住「空值 = 不知道」这条约定：hosting 缺失、status 失败、回包不是
// JSON 都必须回空字段，绝不能退回「大概率住宅」之类的猜测——这份画像会被人用来判断
// 「是不是同一个机房一起出事」，猜错比空着更坏。
func TestClassifyExit(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		raw  string
		want ExitInfo
	}{
		{
			name: "机房",
			raw:  `{"status":"success","query":"203.0.113.7","country":"美国","countryCode":"us","isp":"Example Hosting","as":"AS64500 Example Hosting LLC","hosting":true}`,
			want: ExitInfo{IP: "203.0.113.7", Country: "美国", CountryCode: "US", Kind: "datacenter", ISP: "Example Hosting", ASN: "AS64500 Example Hosting LLC", At: now},
		},
		{
			name: "住宅（isp 缺失时用 org 兜底）",
			raw:  `{"status":"success","query":"198.51.100.9","country":"日本","countryCode":"jp","org":"Home Fibre","hosting":false}`,
			want: ExitInfo{IP: "198.51.100.9", Country: "日本", CountryCode: "JP", Kind: "residential", ISP: "Home Fibre", At: now},
		},
		{
			name: "hosting 缺失：类型空着，其余照给",
			raw:  `{"status":"success","query":"192.0.2.1","country":"德国","countryCode":"de","isp":"Some ISP"}`,
			want: ExitInfo{IP: "192.0.2.1", Country: "德国", CountryCode: "DE", ISP: "Some ISP", At: now},
		},
		{
			name: "限流/私网等失败回包：全空",
			raw:  `{"status":"fail","message":"private range"}`,
			want: ExitInfo{},
		},
		{
			name: "非 JSON",
			raw:  `<html>429</html>`,
			want: ExitInfo{},
		},
	}
	for _, c := range cases {
		if got := classifyExit([]byte(c.raw), now); got != c.want {
			t.Errorf("%s: classifyExit=%+v want %+v", c.name, got, c.want)
		}
	}
}

// TestExitInfoFresh 画像按 intelTTL 过期；零值（从没查过）算过期。
func TestExitInfoFresh(t *testing.T) {
	now := time.Now()
	if (ExitInfo{}).fresh(now) {
		t.Error("零值画像应算过期（还没查过）")
	}
	if !(ExitInfo{At: now.Add(-time.Hour)}).fresh(now) {
		t.Error("1 小时前的画像仍在缓存期内")
	}
	if (ExitInfo{At: now.Add(-2 * intelTTL)}).fresh(now) {
		t.Error("超过 intelTTL 应过期")
	}
}

// 画像必须经被描述的那个代理去查：直连查到的是网关自己的出口，每个节点都会显示同一个地址。
// 开关关着（缺省）时一个第三方请求都不发。
func TestProbeLooksUpExitThroughProxy(t *testing.T) {
	var direct, viaProxy atomic.Int32
	intel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		_, _ = w.Write([]byte(`{"status":"success","query":"198.51.100.1"}`))
	}))
	defer intel.Close()
	old := intelURL
	intelURL = intel.URL + "/"
	t.Cleanup(func() { intelURL = old })
	intelHost := strings.TrimPrefix(intel.URL, "http://")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == intelHost {
			viaProxy.Add(1)
			_, _ = w.Write([]byte(`{"status":"success","query":"203.0.113.9","countryCode":"jp","isp":"Example Cloud","hosting":true}`))
			return
		}
		w.WriteHeader(http.StatusNoContent) // 健康检查
	}))
	defer proxy.Close()

	m := testManager(t, "")
	id := addProxy(t, m, proxy.URL)
	probe(t, m, id)
	if viaProxy.Load()+direct.Load() != 0 {
		t.Fatal("exit lookup ran while disabled")
	}
	m.SetExitLookup(true)
	probe(t, m, id)
	if viaProxy.Load() != 1 || direct.Load() != 0 {
		t.Fatalf("via proxy=%d direct=%d", viaProxy.Load(), direct.Load())
	}
	if got := m.Snapshot().Proxies[0].Exit; got.IP != "203.0.113.9" || got.Kind != "datacenter" {
		t.Fatalf("exit = %+v", got)
	}
	probe(t, m, id) // 6 小时内不重查
	if viaProxy.Load() != 1 {
		t.Fatalf("lookup repeated within TTL: %d", viaProxy.Load())
	}
}
