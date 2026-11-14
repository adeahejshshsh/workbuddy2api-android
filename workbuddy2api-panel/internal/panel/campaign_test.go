package panel

// campaign_test.go 钉住活动端点的两个行为：progress 逐号合并、dryRun 默认不领。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const campaignTasksJSON = `{"code":0,"msg":"OK","data":{
 "invite":{"count":2,"need":3,"open":false},
 "progress":[{"task_code":"ai_chat","progress":3}],
 "tasks":[
  {"task_code":"ai_chat","title":"简单任务，AI 搞定","target_count":3,"reward_credit":100,"need_view":true,"enabled":1},
  {"task_code":"expert","title":"复杂任务，专家帮忙","target_count":1,"reward_credit":100,"need_view":true,"enabled":1}]}}`

const campaignConfigJSON = `{"code":0,"msg":"OK","data":{"config":{"title":"星巴克协作","in_period":true}}}`

func campaignPanel(t *testing.T, claimed *atomic.Int32) *Panel {
	t.Helper()
	// claimedUIDs 哪些号已经领过（进度按号独立，夹具也要按号记）。
	var claimedUIDs sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/config"):
			_, _ = w.Write([]byte(campaignConfigJSON))
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			// 领过之后服务端会在 progress 里写 claimed_at；面板据此判定「真入账」
			// （CampaignClaimVerified），所以夹具必须跟着变，否则回读永远为空。
			// 按账号记（X-User-Id）：进度是**每号独立**的，共用一个开关会让第二个号
			// 看到第一个号的 claimed_at 而整轮跳过。
			body := campaignTasksJSON
			if _, done := claimedUIDs.Load(r.Header.Get("X-User-Id")); done {
				body = strings.Replace(body,
					`{"task_code":"ai_chat","progress":3}`,
					`{"task_code":"ai_chat","progress":3,"claimed_at":"2026-11-12T09:05:00+08:00"}`, 1)
			}
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/vouchers"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"items":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/viewed"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"ok":true}}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claimed.Add(1)
			claimedUIDs.Store(r.Header.Get("X-User-Id"), true)
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.HTTP, up.ChatBaseCN = srv.Client(), srv.URL
	p := pool.New("")
	p.Add(testRealmAuth(t, "cn-1", "cn"))
	return New(Config{Pool: p, Upstream: up})
}

func TestCampaignStatusExposesReady(t *testing.T) {
	var claimed atomic.Int32
	p := campaignPanel(t, &claimed)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/campaign", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []struct {
			UID      string   `json:"uid"`
			InPeriod bool     `json:"in_period"`
			Ready    []string `json:"ready"`
			Tasks    []struct {
				Code     string `json:"task_code"`
				Progress int    `json:"progress"`
			} `json:"tasks"`
		} `json:"accounts"`
		Ready int `json:"accounts_ready"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 || !out.Accounts[0].InPeriod {
		t.Fatalf("accounts=%+v", out.Accounts)
	}
	// ai_chat 3/3 → ready；expert 0/1 → 不 ready
	if len(out.Accounts[0].Ready) != 1 || out.Accounts[0].Ready[0] != "ai_chat" {
		t.Fatalf("ready=%v want [ai_chat]", out.Accounts[0].Ready)
	}
	if out.Ready != 1 {
		t.Fatalf("accounts_ready=%d want 1", out.Ready)
	}
	if claimed.Load() != 0 {
		t.Fatal("读接口不应触发领奖")
	}
}

func TestCampaignClaimDryRunByDefault(t *testing.T) {
	var claimed atomic.Int32
	p := campaignPanel(t, &claimed)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/campaign/claim", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		DryRun bool `json:"dry_run"`
		Rows   []struct {
			Would   []string `json:"would_claim"`
			Claimed []string `json:"claimed"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.DryRun {
		t.Fatal("缺省必须 dry_run=true（不显式确认就不动手）")
	}
	if claimed.Load() != 0 {
		t.Fatalf("dry run 却真的领了 %d 次", claimed.Load())
	}
	if len(out.Rows) != 1 || len(out.Rows[0].Would) != 1 || out.Rows[0].Would[0] != "ai_chat" {
		t.Fatalf("rows=%+v", out.Rows)
	}
}

func TestCampaignClaimRealRunClaimsReadyOnly(t *testing.T) {
	var claimed atomic.Int32
	p := campaignPanel(t, &claimed)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/campaign/claim",
		strings.NewReader(`{"dry_run":false,"uids":["cn-1"]}`))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Rows []struct {
			Claimed []string `json:"claimed"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// 只有 ai_chat 达标；expert 0/1 不该被 claim
	if claimed.Load() != 1 {
		t.Fatalf("claim 次数=%d want 1（只领达标的 ai_chat）", claimed.Load())
	}
	if len(out.Rows) != 1 || len(out.Rows[0].Claimed) != 1 || out.Rows[0].Claimed[0] != "ai_chat" {
		t.Fatalf("rows=%+v", out.Rows)
	}
}

// 进度查询与领奖共用一把锁；短时间内重复查询走缓存，不再把全池扫一遍。
func TestCampaignStatusSerializedAndCached(t *testing.T) {
	var claimed atomic.Int32
	p := campaignPanel(t, &claimed)
	get := func(q string) int {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/campaign"+q, nil))
		return rec.Code
	}
	if c := get(""); c != http.StatusOK {
		t.Fatalf("first read: %d", c)
	}
	p.campaignMu.Lock() // 模拟一轮领奖正在跑
	defer p.campaignMu.Unlock()
	if c := get(""); c != http.StatusOK {
		t.Fatalf("cached read while a run holds the lock: %d", c)
	}
	if c := get("?code=other_campaign"); c != http.StatusConflict {
		t.Fatalf("uncached read while a run holds the lock: %d", c)
	}
}

func postCampaignClaim(p *Panel, ctx context.Context, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/campaign/claim", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)
	return rec
}

// 真领的护栏：不列 uids、module 里带路径字符、已有一轮在跑，都不发任何上游写调用。
func TestCampaignClaimGuards(t *testing.T) {
	var claimed atomic.Int32
	p := campaignPanel(t, &claimed)
	ctx := context.Background()
	if rec := postCampaignClaim(p, ctx, `{"dry_run":false}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("real run without uids: %d", rec.Code)
	}
	if rec := postCampaignClaim(p, ctx, `{"module":"../growth","uids":["cn-1"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("path in module: %d", rec.Code)
	}
	p.campaignMu.Lock()
	rec := postCampaignClaim(p, ctx, `{"dry_run":false,"uids":["cn-1"]}`)
	p.campaignMu.Unlock()
	if rec.Code != http.StatusConflict {
		t.Fatalf("concurrent run: %d", rec.Code)
	}
	if claimed.Load() != 0 {
		t.Fatalf("guarded requests claimed %d times", claimed.Load())
	}
}

// 号与号之间至少隔 campaignClaimGap。
func TestCampaignClaimPacesAccounts(t *testing.T) {
	old := campaignClaimGap
	campaignClaimGap = 300 * time.Millisecond
	t.Cleanup(func() { campaignClaimGap = old })
	var claimed atomic.Int32
	p := campaignPanel(t, &claimed)
	p.cfg.Pool.Add(testRealmAuth(t, "cn-2", "cn"))
	start := time.Now()
	postCampaignClaim(p, context.Background(), `{"dry_run":false,"uids":["cn-1","cn-2"]}`)
	if n, d := claimed.Load(), time.Since(start); n != 2 || d < 300*time.Millisecond {
		t.Fatalf("claims=%d in %v, want 2 spaced by >= 300ms", n, d)
	}
}

// 客户端断开后不再领下一个号，也不在间隔里干等。
func TestCampaignClaimPacesAndStopsOnDisconnect(t *testing.T) {
	old := campaignClaimGap
	campaignClaimGap = 5 * time.Second
	t.Cleanup(func() { campaignClaimGap = old })
	var claimed atomic.Int32
	p := campaignPanel(t, &claimed)
	p.cfg.Pool.Add(testRealmAuth(t, "cn-2", "cn"))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for claimed.Load() == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	postCampaignClaim(p, ctx, `{"dry_run":false,"uids":["cn-1","cn-2"]}`)
	if n := claimed.Load(); n != 1 {
		t.Fatalf("claims = %d, want 1 (second account waits for the gap, then the client is gone)", n)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("handler kept waiting %v after the client left", d)
	}
}
