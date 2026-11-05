package server

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// imageFake 记录图像请求落到哪个 base、body 里的 model，并按脚本回信封。
type imageFake struct {
	paths  []string
	hosts  []string
	bodies []string
	status int
	body   string
}

func (f *imageFake) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			f.paths = append(f.paths, r.URL.Path)
			f.hosts = append(f.hosts, r.URL.Host)
			f.bodies = append(f.bodies, string(raw))
			return &http.Response{
				StatusCode: f.status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(f.body)),
			}, nil
		})},
		ChatBaseCN:     "https://cn.example",
		BillingBaseCN:  "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
}

const imageOK = `{"code":0,"msg":"OK","data":{"created":1791104365,"data":[{"url":"https://cos.example/img.png?sig=x"}],` +
	`"usage":{"input_tokens":336,"output_tokens":518,"total_tokens":854,"credit":5.71}}}`

func imagePool() *pool.Pool {
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	return p
}

// TestImagesGenerations 文生图端点：模型前缀定 realm（前缀不落上游）、信封原样透出、
// 上游错误按其状态码与原文透出、缺 prompt 400。
func TestImagesGenerations(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)))
		return rec
	}

	// ① 国内：cn: 前缀剥掉进上游，回程 data 原样（含 usage/credit）
	rec := post(`{"model":"cn:hunyuan-image-alpha","prompt":"一只猫","n":1,"size":"1024x1024"}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.paths[0] != "/v2/images/generations" || f.hosts[0] != "cn.example" {
		t.Fatalf("落点 %s%s want /v2/images/generations @ cn.example", f.hosts[0], f.paths[0])
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	if sent["model"] != "hunyuan-image-alpha" {
		t.Fatalf("上游收到的 model=%v want 裸名（前缀是网关路由协议）", sent["model"])
	}
	if sent["size"] != "1024x1024" {
		t.Fatalf("其余字段应原样透传，size=%v", sent["size"])
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("回程不是 JSON：%s", rec.Body.String())
	}
	items, _ := got["data"].([]any)
	if len(items) != 1 || !strings.Contains(rec.Body.String(), "https://cos.example/img.png") {
		t.Fatalf("回程应原样带出 url：%s", rec.Body.String())
	}
	if u, _ := got["usage"].(map[string]any); u == nil || u["credit"] != 5.71 {
		t.Fatalf("回程应保留上游 usage/credit：%v", got["usage"])
	}

	// ② 国际：global: 前缀 → 走 global base（混元在国际版无路由，必须能落到对的池）
	f.paths, f.hosts, f.bodies = nil, nil, nil
	if rec := post(`{"model":"global:gpt-image-2.5-sunburst","prompt":"a cat"}`); rec.Code != 200 {
		t.Fatalf("global 出图 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.hosts[0] != "global.example" {
		t.Fatalf("global 应走 ChatBaseGlobal，实际 %s", f.hosts[0])
	}

	// ③ 上游信封错误（国际版拿混元调 = 实测 14401）：状态码与原文都透出
	f.status, f.body = 400, `{"code":14401,"msg":"Create image failed with error: Image model [hunyuan-image-alpha] route config not found"}`
	rec = post(`{"model":"global:hunyuan-image-alpha","prompt":"a cat"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "14401") {
		t.Fatalf("上游错误应透出其状态码与原文：code=%d body=%s", rec.Code, rec.Body.String())
	}

	// ④ 缺 prompt → 400；缺 model 用缺省 cn 图像模型
	if rec := post(`{"model":"cn:hunyuan-image-alpha"}`); rec.Code != 400 {
		t.Fatalf("缺 prompt 应 400，实际 %d", rec.Code)
	}
	f.status, f.body = 200, imageOK
	f.paths, f.bodies = nil, nil
	if rec := post(`{"prompt":"一只猫"}`); rec.Code != 200 {
		t.Fatalf("缺省模型应可用，实际 %d %s", rec.Code, rec.Body.String())
	}
	var def map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &def)
	if def["model"] != "hunyuan-image-alpha" {
		t.Fatalf("缺省模型=%v want hunyuan-image-alpha", def["model"])
	}
}

// TestImagesGenerationsNoAccountForRealm 该 realm 没有可用账号时明确报错（而不是回落到另一域）。
func TestImagesGenerationsNoAccountForRealm(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	h := NewHandler(Config{Pool: p, Upstream: f.client()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"global:gpt-image-2.5-sunburst","prompt":"a cat"}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	if len(f.paths) != 0 {
		t.Fatalf("不该发起上游调用，实际落到 %v", f.paths)
	}
}

// TestImagesRecordedInPanel 出图必须进面板可见的三本账：请求台账事件（请求记录/归档）、
// 模型成本账本（积分）、用量时序。用 RequestLog 的快照断言事件里的账号/模型/积分。
func TestImagesRecordedInPanel(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:             imagePool(),
		Upstream:         f.client(),
		RequestLog:       reqLog,
		RecordClientInfo: true, // 来源采集与 chat 同开关
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"一只猫"}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	req.Header.Set("User-Agent", "test-client/1.0")
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Request-Id"); !strings.HasPrefix(got, "req-") {
		t.Fatalf("图像请求也应有 X-Request-Id，实际 %q", got)
	}
	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("请求台账应记 1 条，实际 %d 条：%+v", len(s.Recent), s.Recent)
	}
	e := s.Recent[0]
	if e.Path != "/v1/images/generations" || e.Model != "hunyuan-image-alpha" || !e.OK {
		t.Fatalf("事件 path/model/ok 不对：%+v", e)
	}
	if e.Account == "" || e.Credit != 5.71 || e.TotalTokens != 854 {
		t.Fatalf("事件应带账号与上游用量（credit/tokens）：%+v", e)
	}
	if e.ClientIP == "" || !strings.Contains(e.UserAgent, "test-client") {
		t.Fatalf("事件应带来源（IP/UA）：%+v", e)
	}
	// 成本账本：出图扣费必须落进账号的模型成本（面板积分构成据此显示）
	st, _ := h.cfg.Pool.Status("c1")
	found := false
	for _, mc := range st.ModelCosts {
		if mc.Model == "hunyuan-image-alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("NoteModelCost 未落账，model_costs=%+v", st.ModelCosts)
	}
}

// TestImagesChargeRecordedPerTier 出图实扣按「尺寸×画质」分档落观测（面板倍率列据此
// 显示「实扣 x/张 + 档位」）。
//
// 两种回执形状都要覆盖：global 回 size/quality（直接采信）；CN 不回（回落请求体）。
// 混档会让 0.55 与 4.84 混成一个假均值——这正是本次修复的靶子。
func TestImagesChargeRecordedPerTier(t *testing.T) {
	cases := []struct {
		name  string
		model string
		body  string
		resp  string
		want  upstream.ImageChargeAgg
	}{
		{
			name:  "global 回执自带 size/quality（high 档 4.84）",
			model: "global:gpt-image-2.5-sunburst",
			body:  `{"model":"global:gpt-image-2.5-sunburst","prompt":"a cat","size":"512x512"}`,
			resp: `{"code":0,"data":{"created":1,"size":"1024x1024","quality":"high",` +
				`"data":[{"b64_json":"AAA"}],"usage":{"output_image_tokens":35333,"output_image_counts":1,"credit":4.84}}}`,
			want: upstream.ImageChargeAgg{Size: "1024x1024", Quality: "high", Last: 4.84, LastTokens: 35333},
		},
		{
			name:  "CN 回执不带 size（回落请求体，每张固定 5.71）",
			model: "cn:hunyuan-image-alpha",
			body:  `{"model":"cn:hunyuan-image-alpha","prompt":"a cat","size":"512x512"}`,
			resp: `{"code":0,"data":{"created":1,"data":[{"url":"https://cos/x.png"}],` +
				`"usage":{"input_tokens":334,"output_tokens":1118,"total_tokens":1452,"output_image_counts":1,"credit":5.71}}}`,
			want: upstream.ImageChargeAgg{Size: "512x512", Quality: "", Last: 5.71, LastTokens: 0},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &imageFake{status: 200, body: c.resp}
			up := f.client()
			h := NewHandler(Config{Pool: imagePool(), Upstream: up})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(c.body))
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			realm, bare := "global", "gpt-image-2.5-sunburst"
			if strings.HasPrefix(c.model, "cn:") {
				realm, bare = "cn", "hunyuan-image-alpha"
			}
			got := up.ImageCharges(realm, bare)
			if len(got) != 1 {
				t.Fatalf("应记 1 档，实际 %+v", got)
			}
			g := got[0]
			if g.Size != c.want.Size || g.Quality != c.want.Quality {
				t.Errorf("档位 size/quality=%q/%q want %q/%q", g.Size, g.Quality, c.want.Size, c.want.Quality)
			}
			if g.Last != c.want.Last || g.LastTokens != c.want.LastTokens {
				t.Errorf("实扣 last/tokens=%v/%d want %v/%d", g.Last, g.LastTokens, c.want.Last, c.want.LastTokens)
			}
			if g.Samples != 1 {
				t.Errorf("samples=%d want 1", g.Samples)
			}
		})
	}
}

// TestImagesEdits 图生图：JSON（data URL）与 multipart（OpenAI SDK 形态）都要能转成
// 上游要的 JSON——image 数组、其余字段透传。
func TestImagesEdits(t *testing.T) {
	f := &imageFake{status: 200, body: imageOK}
	h := NewHandler(Config{Pool: imagePool(), Upstream: f.client()})

	// ① JSON：image 单值归一成数组
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"改成蓝色","image":"data:image/png;base64,AAAA"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.paths[0] != "/v2/images/edits" {
		t.Fatalf("应打上游 edits 路由，实际 %s", f.paths[0])
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	if imgs, _ := sent["image"].([]any); len(imgs) != 1 || imgs[0] != "data:image/png;base64,AAAA" {
		t.Fatalf("image 应归一成数组：%v", sent["image"])
	}

	// ② multipart：文件字段转 data URL
	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "cn:hunyuan-image-alpha")
	_ = mw.WriteField("prompt", "改成蓝色")
	_ = mw.WriteField("n", "1")
	fw, _ := mw.CreateFormFile("image", "in.png")
	_, _ = fw.Write([]byte("PNGDATA"))
	_ = mw.Close()
	f.paths, f.bodies = nil, nil
	rd := httptest.NewRequest("POST", "/v1/images/edits", strings.NewReader(buf.String()))
	rd.Header.Set("Content-Type", mw.FormDataContentType())
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, rd)
	if rec.Code != 200 {
		t.Fatalf("multipart code=%d body=%s", rec.Code, rec.Body.String())
	}
	var sent2 map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent2)
	imgs, _ := sent2["image"].([]any)
	// 文件字段转 data URL：mime 用上传时声明的（Go 的 CreateFormFile 默认
	// application/octet-stream）；载荷是 base64 原文。
	if len(imgs) != 1 || !strings.HasPrefix(imgs[0].(string), "data:") ||
		!strings.Contains(imgs[0].(string), ";base64,UE5HREFUQQ==") {
		t.Fatalf("multipart 的 image 应转成 data URL：%v", sent2["image"])
	}
	if sent2["n"] != float64(1) || sent2["prompt"] != "改成蓝色" {
		t.Fatalf("multipart 其余字段应透传且 n 转数字：%v", sent2)
	}

	// ③ 缺图 → 400（图生图必须有 image）
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/edits",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"x"}`)))
	if rec.Code != 400 {
		t.Fatalf("缺 image 应 400，实际 %d", rec.Code)
	}
}

// TestImagesRetryRotatesAccount 出图失败要换号：第一个号吃 429 软限流后应自动换第二个号
// 成功，且失败号被冷却（下次选号跳过）。
func TestImagesRetryRotatesAccount(t *testing.T) {
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			authz := r.Header.Get("Authorization")
			if strings.Contains(authz, "at-bad") {
				return &http.Response{StatusCode: 429,
					Header: http.Header{"Content-Type": []string{"application/json"}},
					Body:   io.NopCloser(strings.NewReader(`{"code":6004,"msg":"rate limited"}`))}, nil
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(imageOK))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	// 两个号：随机源固定为 0 时先选 bad（插入顺序），失败后不应再选它
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
	)
	// 让「先选 bad」成为确定事件：本仓的选号在**等权重并列段内**会用 time-seeded 源
	// 洗牌（pick.go「并列段内再洗一次」，防止等权重账号按 uid 字典序被饿死），
	// 所以只靠「随机源固定为 0」定不住先选谁 —— 给 bad 一个数量级的余额优势即可让
	// 两者权重不再并列，排序与抽签都确定。
	p.SetCredits("bad", 100000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"cn:hunyuan-image-alpha","prompt":"一只猫"}`)))
	if rec.Code != 200 {
		t.Fatalf("换号后应成功：code=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("应恰好 2 次上游调用（坏号 + 好号），实际 %d", calls)
	}
	bad, _ := p.Status("bad")
	// 上游给的是 6004（模型级）→ 停的是该 (号,模型)，账号本身不被整体冷却。
	// 轮转照旧发生（换号后成功），两条断言一起钉住「换号 + 只停模型」。
	if bad.Cooling {
		t.Fatalf("6004 是模型级限流，账号不得整体冷却：%+v", bad)
	}
	if len(bad.RateLimitedModels) != 1 || bad.RateLimitedModels[0].Model != "hunyuan-image-alpha" {
		t.Fatalf("该 (号,模型) 应被停车：%+v", bad.RateLimitedModels)
	}
}

// TestImagesFailFastOnAccountIndependentErrors 出图侧请求级/模型级错误必须立即透传，
// 不再按账号数换号重试（本轮修复）。
//
// 为什么：实测 14401「route config not found」会打满池里每个账号（2 个号 → 2 次上游
// 调用），客户端还因 502 当瞬时故障反复重试；而它换任何账号都是同一个答复——模型在该
// 域没有路由是域级配置。11101 同理（body 本身畸形）。所以断言「只打一次上游」。
func TestImagesFailFastOnAccountIndependentErrors(t *testing.T) {
	cases := []struct {
		name  string
		model string
		body  string
		want  string
		// 两个同域账号，才能证明没换号
		accts []*auth.Auth
	}{
		{
			name:  "14401 模型在本域无路由",
			model: "global:hunyuan-image-alpha",
			body:  `{"code":14401,"msg":"Create image failed with error: Image model [hunyuan-image-alpha] route config not found"}`,
			want:  "14401",
			accts: []*auth.Auth{
				{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
				{UID: "g2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
			},
		},
		{
			name:  "11101 请求体被上游拒",
			model: "cn:hunyuan-image-alpha",
			body:  `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`,
			want:  "11101",
			accts: []*auth.Auth{
				{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
				{UID: "c2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &imageFake{status: http.StatusBadRequest, body: c.body}
			h := NewHandler(Config{Pool: testPoolWith(c.accts...), Upstream: f.client()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
				strings.NewReader(`{"model":"`+c.model+`","prompt":"a cat"}`)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s (want 400 透传)", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("400 应携带上游原文（%s）：%s", c.want, rec.Body)
			}
			if len(f.paths) != 1 {
				t.Errorf("与账号无关的错误不得换号重试：打了 %d 次上游（want 1）", len(f.paths))
			}
		})
	}
}

// TestGenerationModelsListedInV1Models 出图模型单列到 /v1/models（image_generation:true）：
// 它们被 nonChatModel 剔出对话列表，不留存就永远发现不了；同时白名单/黑名单要能管住它，
// 否则「隐藏了却还列给客户端」等于没隐藏。
func TestGenerationModelsListedInV1Models(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{}`)),
				Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	// 直接往旁路留存里塞一批（等价于一次目录解析的结果）：
	// 一张图、一段视频（视频现在有 /v1/videos/* 端点，要列）、一个普通对话模型（不该单列）。
	up.StashGenerationModelsForTest("cn", []upstream.ModelInfo{
		{ID: "hunyuan-image-alpha", GenerationKind: "image", Tags: []string{"text-to-image"}},
		{ID: "kling-video-v3", GenerationKind: "video", Tags: []string{"text-to-video"}},
		{ID: "glm-5.2"},
	})
	build := func(cfg Config) []map[string]any {
		cfg.Pool = testPoolWith()
		cfg.Upstream = up
		return NewHandler(cfg).generationModelEntries()
	}

	// 图片与视频都单列（各自带自己的标记），普通对话模型不在这里出现。
	got := build(Config{})
	if len(got) != 2 {
		t.Fatalf("图片与视频都该列出：%v", got)
	}
	byID := map[string]map[string]any{}
	for _, e := range got {
		byID[e["id"].(string)] = e
	}
	if e := byID["cn:hunyuan-image-alpha"]; e == nil || e["image_generation"] != true {
		t.Fatalf("出图模型应带 image_generation: %v", got)
	}
	if e := byID["cn:kling-video-v3"]; e == nil || e["video_generation"] != true {
		t.Fatalf("出视频模型应带 video_generation: %v", got)
	}
	if _, listed := byID["cn:glm-5.2"]; listed {
		t.Fatalf("普通对话模型不该出现在媒体单列里：%v", got)
	}

	// 黑名单命中 → 不列（图片与视频同一条规则）。
	if l := build(Config{ModelsHidden: []string{"cn:hunyuan-image-alpha", "cn:kling-video-v3"}}); len(l) != 0 {
		t.Fatalf("黑名单里的媒体模型不该列出：%v", l)
	}
	// 白名单启用但没列它 → 不列；列了 → 列出。
	if l := build(Config{ModelsAllowed: []string{"glm-5.2"}}); len(l) != 0 {
		t.Fatalf("白名单外的媒体模型不该列出：%v", l)
	}
	if l := build(Config{ModelsAllowed: []string{"cn:hunyuan-image-alpha", "cn:kling-video-v3"}}); len(l) != 2 {
		t.Fatalf("白名单内的媒体模型应列出：%v", l)
	}
}
