package upstream

// audit_p2_test.go 本轮审查（P2 主题：静默截断 / 错误吞没 / 无界增长 / 计费口径）的回归网。
//
// 每个用例都对应一条**修复前会失败**的断言，用例名里带审查编号，便于回溯到
// CODE-AUDIT-FULL.md 的 10.5–10.8 节。写用例时踩到的两个共性：
//
//  1. 「截断」类缺陷不能靠「解析失败」间接发现 —— LimitReader 读到上限就 EOF，
//     与「上游刚好发满」不可区分。要证明修复生效，必须让上游真的发**超过上限**
//     的字节，并断言错误里能看出「是我们砍的」。
//  2. 「错误吞没」类缺陷要构造**能过外层信封、但在内层解析失败**的报文
//     （例如 {"code":0,"data":123}）—— 否则 doJSON 自己就先报错了，走不到
//     被吞掉的那一行，用例会假绿。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// errReadCloser 读即报错的 body：模拟传输中途断流（A1-21 的触发条件）。
type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("boom-read") }
func (errReadCloser) Close() error             { return nil }

// bigJSONBody 生成一个远超上限、且**看起来像 JSON** 的 body：
// 前缀是合法 JSON 开头，后面填充到 n 字节。截断检测必须靠「读超了」而不是
// 「解析失败」才能区分「网关砍的」与「上游给的坏 JSON」。
func bigJSONBody(n int) string {
	var b strings.Builder
	b.WriteString(`{"code":0,"data":{"blob":"`)
	for b.Len() < n {
		b.WriteString(strings.Repeat("x", 4096))
	}
	b.WriteString(`"}}`)
	return b.String()
}

// ---------------------------------------------------------------------------
// 静默截断（A1-15 / W4-U2-104）
// ---------------------------------------------------------------------------

// readAllLimited 的核心契约：读满 max 与读超 max 必须可区分。
func TestReadAllLimitedDetectsTruncation(t *testing.T) {
	exact := strings.Repeat("a", 16)
	raw, truncated, err := readAllLimited(strings.NewReader(exact), 16)
	if err != nil || truncated || len(raw) != 16 {
		t.Fatalf("刚好读满不该判截断: len=%d truncated=%v err=%v", len(raw), truncated, err)
	}

	raw, truncated, err = readAllLimited(strings.NewReader(exact+"b"), 16)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(raw) != 16 {
		t.Fatalf("多 1 字节必须判截断: len=%d truncated=%v", len(raw), truncated)
	}

	if _, err := readAllLimitedStrict(strings.NewReader(exact+"b"), 16); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("strict 版必须转成 ErrResponseTooLarge，得到 %v", err)
	}
}

// doJSONRaw 撞 1MiB 上限必须报「截断」，不能报成「上游 JSON 坏了」。
func TestDoJSONRawReportsTruncation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(bigJSONBody(1<<20 + 4096))),
		}, nil
	})
	req, err := http.NewRequest(http.MethodPost, "https://billing.example/v2/report", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.doJSONRaw(req)
	if err == nil {
		t.Fatal("超 1MiB 必须报错")
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("必须能看出是网关截断（ErrResponseTooLarge），得到 %v", err)
	}
	if strings.Contains(err.Error(), "parse failed") {
		t.Fatalf("不该被归因成解析失败: %v", err)
	}
}

// search/fetch 撞 1MiB 上限：此前把半截字符串当 json.RawMessage **成功**返回，
// 客户端拿到非法 JSON（W4-U2-104）。
func TestSearchTruncationIsError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(bigJSONBody(1<<20 + 4096))),
		}, nil
	})
	_, err := c.WebFetch(context.Background(), &auth.Auth{AccessToken: "at"}, "https://x.example")
	if err == nil {
		t.Fatal("超限必须报错，不能把截断的 body 当成功返回")
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("错误里要能看出是网关截断，得到 %v", err)
	}
}

// 3D 的 4MiB 截断同样要显式（W4-U2-064）：此前与「上游返回错误页」混在同一个
// 解析失败分支里，只能看到前 300 字符。
func TestThreeDTruncationIsExplicit(t *testing.T) {
	c := newThreeDClient(t, map[string]string{"*": "ck_t_manual"}, rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("y", 4<<20+1024))),
		}, nil
	}))
	_, err := c.ThreeDTask(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "job")
	if err == nil || !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("4MiB 截断必须显式报错，得到 %v", err)
	}
}

// 读失败不能被吞掉后变成「解析失败」（A1-21）：真因是传输中断。
func TestGlobalRegisterReadErrorNotSwallowed(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: errReadCloser{}}, nil
	})
	req, err := http.NewRequest(http.MethodPost, "https://register.example/x", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = c.globalRegisterJSON(req)
	if err == nil {
		t.Fatal("读失败必须报错")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Fatalf("必须报读取失败（真因），而不是 parse: %v", err)
	}
	if strings.Contains(err.Error(), "parse") {
		t.Fatalf("不该被归因成解析失败: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 错误吞没（W4-U2-068 / W4-U2-101 / 内层解析）
// ---------------------------------------------------------------------------

// acpPost：HTTP 200 + JSON-RPC error 必须当失败（W4-U2-068）。
// 此前只判 HTTP 状态，上游的「会话不存在/模型不可用」被吞成成功，
// 四步全"过"，最后只在轮询超时时报「没跑完」。
func TestACPPostJSONRPCErrorIsFailure(t *testing.T) {
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"session not found"}}`), nil
	})}, ChatBaseCN: "https://cn.example"}
	sess := WebSession{Link: "https://sandbox.example/acp", Token: "sandbox-token"}
	err := c.acpPost(context.Background(), sess, "conn-1", 2, "session/load", map[string]any{})
	if err == nil {
		t.Fatal("JSON-RPC error 必须返回错误")
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("错误里要带上上游 message，得到 %v", err)
	}
}

// 反向保护：`"error":null` 是 OpenAI 兼容实现的常态，不能判成失败
// （否则 initialize 永远失败、整条网页通道全废）。
func TestACPPostJSONRPCNullErrorIsSuccess(t *testing.T) {
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"jsonrpc":"2.0","id":1,"result":{},"error":null}`), nil
	})}, ChatBaseCN: "https://cn.example"}
	sess := WebSession{Link: "https://sandbox.example/acp", Token: "sandbox-token"}
	if err := c.acpPost(context.Background(), sess, "conn-1", 1, "initialize", map[string]any{}); err != nil {
		t.Fatalf(`"error":null 不该判失败: %v`, err)
	}
}

// 空 body（202 无内容）也必须算成功：真实上游对 ACP POST 回 202 空体。
func TestACPPostEmptyBodyIsSuccess(t *testing.T) {
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}, ChatBaseCN: "https://cn.example"}
	sess := WebSession{Link: "https://sandbox.example/acp", Token: "sandbox-token"}
	if err := c.acpPost(context.Background(), sess, "conn-1", 1, "initialize", map[string]any{}); err != nil {
		t.Fatalf("202 空体不该判失败: %v", err)
	}
}

// CompensationStatus 的内层解析失败必须报错（W4-U2-101 第三处）：
// 此前 `_ = json.Unmarshal` 让「上游回了一坨非对象」与「当前没有补偿活动」
// 完全同形 —— 带截止日的补偿会静默过期。
func TestCompensationStatusParseErrorReported(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		// 外层信封合法（code=0），内层 data 不是对象 → 只有被吞掉的那一行会失败。
		return jsonResp(200, `{"code":0,"data":123}`), nil
	})
	_, err := c.CompensationStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err == nil {
		t.Fatal("内层解析失败必须报错，不能当成「没有补偿活动」")
	}
}

// TravelClaim 同理：奖励字段缺失是合法的（解出来是 0），但**报文解不出**必须报。
func TestTravelClaimParseErrorReported(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":123}`), nil
	})
	if _, err := c.TravelClaim(&auth.Auth{UID: "u1", AccessToken: "at"}, 7); err == nil {
		t.Fatal("内层解析失败必须报错，不能返回「0 积分 + nil」")
	}
}

// embeddings：200 + 非 JSON（网关插的 HTML 告警页）此前被当成功透传。
func TestEmbeddingsNonJSONBodyIsNotSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader("<html><body>gateway</body></html>")),
		}, nil
	})
	raw, err := c.Embeddings(context.Background(), &auth.Auth{AccessToken: "at"}, "m", []string{"x"})
	if err == nil {
		t.Fatalf("非 JSON 的 200 不能当向量成功返回: %q", string(raw))
	}
}

// ---------------------------------------------------------------------------
// 无界增长 / 兜底被永久钉死
// ---------------------------------------------------------------------------

// models.dev 返回「能解成对象但没有条目」的响应时，不能把空索引落库
// （W4-U2-054）：`f.doc != nil` 是 ensureDocAsync 的短路条件，一次 CDN 错误页
// 就会让兜底在本进程内彻底失效。
func TestModelsDevEmptyDocNotPinned(t *testing.T) {
	f := &modelsDevFetcher{}
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		// 结构完全合法、但**一条模型都没有**（CDN 错误页 / 被劫持返回 {} 的形态）。
		// 这类响应能被 Unmarshal 成对象 → 得到非 nil 空 map → 若落库就永久短路兜底。
		return jsonResp(200, `{"openai":{"models":{}}}`), nil
	})}
	f.fetchDoc(client, "https://models.dev.example/api.json")
	f.mu.Lock()
	doc := f.doc
	f.mu.Unlock()
	if doc != nil {
		t.Fatalf("空索引不该落库（否则冷却重试永久失效），得到 %d 条", len(doc))
	}
}

// 兜底路径的 goroutine 里 panic 不能打死进程（W4-U2-055）。
func TestModelsDevFetchDocSurvivesPanic(t *testing.T) {
	f := &modelsDevFetcher{}
	client := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		panic("roundtripper boom")
	})}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("fetchDoc 必须自己 recover（它只是可选兜底，无权打死进程）: %v", r)
		}
	}()
	f.fetchDoc(client, "https://models.dev.example/api.json")
}

// 3D 临时凭证缓存要摊销清理过期条目（W4-U2-067）：唯一的删除点原先只有
// Invalidate（上游 401/403），过期条目在进程生命周期内只增不减。
func TestAutoThreeDCachePrunesExpired(t *testing.T) {
	fetchCalls := 0
	auto := NewAutoThreeDCredentials(func(context.Context, *auth.Auth) (string, time.Time, error) {
		fetchCalls++
		return "ck_t_fresh", time.Now().Add(time.Hour), nil
	}, nil)
	// 预置超过软上限的**已过期**条目：它们命中不了，纯占内存。
	past := time.Now().Add(-time.Hour)
	for i := 0; i < autoKeyCacheSoftCap+8; i++ {
		auto.cache[string(rune('a'+i%26))+string(rune('a'+i/26))] = autoKeyEntry{key: "old", expiresAt: past}
	}
	if _, err := auto.TempToken(context.Background(), &auth.Auth{UID: "fresh-uid"}); err != nil {
		t.Fatal(err)
	}
	if fetchCalls != 1 {
		t.Fatalf("fetch 次数=%d want 1", fetchCalls)
	}
	auto.mu.Lock()
	n := len(auto.cache)
	auto.mu.Unlock()
	if n != 1 {
		t.Fatalf("过期条目应被摊销清理，剩 %d 条（只该剩刚写入的那 1 条）", n)
	}
}

// ---------------------------------------------------------------------------
// 计费与归档口径
// ---------------------------------------------------------------------------

// Aggregate 的 usage 不能被末帧的空 `{}` 抹掉（F-13 / W4-U1-40）：
// 上游「先发数值 usage、末帧再发空 usage」时，非流式响应会丢整份用量 → 计费记 0。
func TestAggregateKeepsUsageWhenEmptyFrameArrives(t *testing.T) {
	const stream = "data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"total_tokens\":14}}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{}}\n\n" +
		"data: [DONE]\n\n"
	got, err := Aggregate(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	usage, _ := got["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("空 usage 帧不该把真实用量抹掉")
	}
	if usage["prompt_tokens"] != 11.0 {
		t.Fatalf("prompt_tokens=%v want 11", usage["prompt_tokens"])
	}
}

// 不凭空造 prompt_tokens_details（W4-U2-087）：与 input_tokens_details 同口径。
func TestNormalizeUsageDoesNotInventPromptDetails(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":           100.0,
		"prompt_cache_hit_tokens": 80.0,
	}
	got := normalizeUsageCacheAliases(usage)
	if _, exists := got["prompt_tokens_details"]; exists {
		t.Fatalf("上游没给 prompt_tokens_details 时不该凭空造: %v", got["prompt_tokens_details"])
	}
	if got["cached_tokens"] != 80.0 {
		t.Fatalf("顶层别名仍要回写，cached_tokens=%v", got["cached_tokens"])
	}
}

// usageNumber 的类型集合要与 positiveUsageNumber 一致（W4-U2-088）：
// 否则同一份 usage 在「命中率统计」与「prompt_tokens 读取」两条路径上一个 ok 一个不 ok。
func TestUsageNumberTypeSetMatchesPositive(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"float64", float64(7)},
		{"float32", float32(7)},
		{"int", 7},
		{"int64", int64(7)},
		{"int32", int32(7)},
		{"uint", uint(7)},
		{"uint64", uint64(7)},
		{"uint32", uint32(7)},
		{"json.Number", json.Number("7")},
	}
	for _, tc := range cases {
		usage := map[string]any{"prompt_tokens": tc.value}
		if got, ok := UsagePromptTokens(usage); !ok || got != 7 {
			t.Fatalf("%s: prompt_tokens=%v ok=%v want 7", tc.name, got, ok)
		}
		if _, ok := positiveUsageNumber(tc.value); !ok {
			t.Fatalf("%s: 两个函数必须支持同一套类型", tc.name)
		}
	}
}

// 字符串小数容量必须四舍五入（A1-07）：截断会把 0.9 分记成 0。
func TestRoundCapacityRoundsInsteadOfTruncating(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{0.9, 1},
		{0.4, 0},
		{335.47000026, 335},
		{1200, 1200},
		{0, 0},
		{-0.6, -1},
	}
	for _, tc := range cases {
		if got := roundCapacity(tc.in); got != tc.want {
			t.Fatalf("roundCapacity(%v)=%d want %d", tc.in, got, tc.want)
		}
	}
}

// globalUserResourceBuckets：summary 回空 Packages 时**不是**「余额归零」观测（A1-03）。
// 此前无条件 notifyCredits，台账会多出一条假流水，下一轮真实余额再产生假 delta。
func TestGlobalResourceEmptyPackagesDoesNotNotifyCredits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		packages string
		want     int
	}{
		{name: "空 Packages 不通知", packages: `[]`, want: 0},
		{name: "有 Packages 正常通知", packages: `[{"PackageCode":"PA","CycleTotalCapacity":"100","CycleRemainCapacity":"60"}]`, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
					return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Packages":`+tc.packages+`}}}}`), nil
				default:
					// 明细端点失败不影响总额（降级为无到期批次）。
					return nil, errors.New("detail down")
				}
			})
			c.GlobalEnabled = true
			var notifications int
			c.SetCreditObserver(func(string, int64) { notifications++ })
			if _, _, _, _, _, _, err := c.globalUserResourceBuckets(globalAuth(), 0); err != nil {
				t.Fatalf("resource: %v", err)
			}
			if notifications != tc.want {
				t.Fatalf("notifyCredits 次数=%d want %d", notifications, tc.want)
			}
		})
	}
}

// 面额未知（size==0）时不能算出负的「已用」（A1-28）：
// `size-remain` 会得到 -50，面板「已用」列与「剩余」自相矛盾。
func TestCreditPackagesUsedNeverNegative(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Packages":[
				{"PackageCode":"UNLIMITED","CycleTotalCapacity":"0","CycleRemainCapacity":"50"}
			]}}}}`), nil
		default:
			return nil, errors.New("detail down")
		}
	})
	c.GlobalEnabled = true
	pkgs, _, _, err := c.CreditPackages(globalAuth())
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("packages=%d want 1", len(pkgs))
	}
	if pkgs[0].Used < 0 {
		t.Fatalf("面额未知时不该给出负的已用: used=%d (remain=%d size=%d)", pkgs[0].Used, pkgs[0].Remain, pkgs[0].Size)
	}
}

// 企业额度 remain 必须钳上界（A1-27）：used 为负脏值时 `limit-used` 会得到
// 「剩余 > 总额」，路由权重与 credit_floor 都会高估这个号。
func TestEnterpriseRemainClampedToLimit(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/get-enterprise-user-usage") {
			return nil, errors.New("unexpected path: " + r.URL.Path)
		}
		return jsonResp(200, `{"code":0,"data":{"limitNum":100,"used_num":-50}}`), nil
	})
	a := &auth.Auth{UID: "e1", AccessToken: "at", EnterpriseID: "ent-1"}
	remain, total, _, _, _, err := c.enterpriseResource(a, 0)
	if err != nil {
		t.Fatalf("enterprise resource: %v", err)
	}
	if remain > total {
		t.Fatalf("剩余(%d) 不该超过总额(%d)：used 负脏值没被钳上界", remain, total)
	}
}

// ---------------------------------------------------------------------------
// 3D 错误分类与凭证回收
// ---------------------------------------------------------------------------

// 信封内的业务错误必须按 400 分类（F-06）：此前先合成 502 再 Classify，
// status>=500 命中 ErrServer → 参数类/鉴权类拒绝被记进账号连败/熔断。
func TestThreeDBusinessErrorIsClientNotServer(t *testing.T) {
	e := threeDError(200, "InvalidParameter: unsupported model")
	if e.Kind == ErrServer {
		t.Fatalf("业务错误不该判成 ErrServer（会冷却健康账号）: kind=%v", e.Kind)
	}
	if e.Status != http.StatusBadRequest {
		t.Fatalf("Status=%d want 400", e.Status)
	}
	if e.HTTPStatus != 200 {
		t.Fatalf("HTTPStatus=%d want 200（真实 HTTP 状态要保留，排障用）", e.HTTPStatus)
	}
	// 真正的 5xx 仍要 ErrServer（否则上游故障被当参数错，号池会一直重试同一个号）。
	if e5 := threeDError(503, "service unavailable"); e5.Kind != ErrServer {
		t.Fatalf("5xx 必须仍是 ErrServer，得到 %v", e5.Kind)
	}
}

// 401 + 非 JSON（HTML 错误页）也要丢缓存凭证（W4-U2-060）：此前该分支直接构造
// *Error 返回，跳过 Invalidate，坏凭证留到 14 分钟 TTL 结束，期间该账号 3D 全失败。
func TestThreeDParseFailureInvalidatesCredential(t *testing.T) {
	fetchCalls, upstreamCalls := 0, 0
	auto := NewAutoThreeDCredentials(func(context.Context, *auth.Auth) (string, time.Time, error) {
		fetchCalls++
		return "ck_t_gen" + string(rune('0'+fetchCalls)), time.Now().Add(time.Hour), nil
	}, nil)
	c := newThreeDClient(t, nil, rtFunc(func(r *http.Request) (*http.Response, error) {
		upstreamCalls++
		if upstreamCalls == 1 {
			return &http.Response{
				StatusCode: 401,
				Header:     http.Header{"Content-Type": []string{"text/html"}},
				Body:       io.NopCloser(strings.NewReader("<html>401 Unauthorized</html>")),
			}, nil
		}
		return jsonResp(200, `{"Response":{"Status":"RUN"}}`), nil
	}))
	c.ThreeDCredential = auto
	a := &auth.Auth{UID: "u-3d", AccessToken: "at", ExpiresAt: 9999999999}
	if _, err := c.ThreeDTask(context.Background(), a, "job"); err == nil {
		t.Fatal("401 必须返回错误")
	}
	if _, err := c.ThreeDTask(context.Background(), a, "job"); err != nil {
		t.Fatalf("第二次必须能用新凭证成功: %v", err)
	}
	if fetchCalls != 2 {
		t.Fatalf("凭证签发次数=%d want 2（解析失败分支也必须 Invalidate）", fetchCalls)
	}
}

// ---------------------------------------------------------------------------
// 协议/边界/可观测
// ---------------------------------------------------------------------------

// BuddyInfo 的 `{}` 等价于「没有猫」（W4-U2-090）：此前解成零值 Buddy 并返回
// 非 nil，调用方按 `b != nil` 判「已有猫」→ 跳过领养，300 分永远领不到。
func TestBuddyInfoEmptyObjectIsNoCat(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"data":{"buddy":{}}}`,
		`{"code":0,"data":{"buddy":{"id":0,"name":""}}}`,
		`{"code":0,"data":{"buddy":null}}`,
		`{"code":0,"data":{}}`,
	} {
		c := testClient(func(r *http.Request) (*http.Response, error) { return jsonResp(200, body), nil })
		b, err := c.BuddyInfo(&auth.Auth{UID: "u1", AccessToken: "at"})
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if b != nil {
			t.Fatalf("%s: 空猫档案应返回 nil，得到 %+v", body, b)
		}
	}
}

// truncateBody 按 rune 截断（A1-19）：按字节切会把多字节字符劈成两半，
// 错误信息里出现乱码，反而看不出上游说了什么。
func TestTruncateBodyKeepsUTF8(t *testing.T) {
	got := truncateBody("中文错误信息", 2)
	if !utf8.ValidString(got) {
		t.Fatalf("截断后必须是合法 UTF-8: %q", got)
	}
	if got != "中文…" {
		t.Fatalf("truncateBody=%q want 中文…", got)
	}
	if short := truncateBody("短", 10); short != "短" {
		t.Fatalf("未超长不该改动: %q", short)
	}
}

// FrameHintFunc(nil) 不能 panic 在 SSE 错误帧路径上（A1-23）。
func TestFrameHintFuncNilContext(t *testing.T) {
	fn := FrameHintFunc(nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ctxFn 为 nil 不该 panic: %v", r)
		}
	}()
	if got := fn(`{"error":{"code":6004,"message":"模型限流"}}`); got == "" {
		t.Fatal("nil ctxFn 时仍应给出 hint")
	}
	if got := fn(""); got != "" {
		t.Fatalf("空 payload 应返回空 hint，得到 %q", got)
	}
}

// shortIDTail 短 id 不能越界（W4-U2-083）：当前 NewConversationID 恒长，
// 但那是上游实现细节，不该成为本包的隐式 panic 契约。
func TestSchoolChatTimesShortID(t *testing.T) {
	if got := shortIDTail("abc"); got != "abc" {
		t.Fatalf("短 id 应整串返回，得到 %q", got)
	}
	if got := shortIDTail("0123456789"); got != "23456789" {
		t.Fatalf("长 id 应取尾 8 位，得到 %q", got)
	}
	ev := SchoolChatTimesEvents("conv-1")
	if msg, _ := ev["messageId"].(string); !strings.HasPrefix(msg, "msg-") {
		t.Fatalf("messageId=%v", ev["messageId"])
	}
}

// agents 名单与 data.models 完全对不上时必须回落全量（A1-04）：
// pick.ordered 在「无任何 id 命中」时返回**非 nil 空切片**，此前只判 nil，
// 于是整个 v3 目录被当成 0 个模型写进 5min 负缓存。
func TestFetchV3ModelsFallsBackWhenAgentsDisjoint(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v3/config") {
			return nil, errors.New("unexpected path: " + r.URL.Path)
		}
		return jsonResp(200, `{"code":0,"data":{
			"models":[{"id":"real-1","name":"Real 1"},{"id":"real-2","name":"Real 2"}],
			"agents":[{"name":"cli","models":["ghost-a","ghost-b"]}]
		}}`), nil
	})
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	models, err := c.fetchV3Models(a)
	if err != nil {
		t.Fatalf("fetchV3Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("agents 与 models 无交集时应回落全量（2 条），得到 %d 条: %+v", len(models), models)
	}
}

// DumpDir 不可写时只告警、照常透传（A1-22 的「写失败不影响请求」这一半）。
func TestDumpUnwritableDirKeepsPassthrough(t *testing.T) {
	c := &Client{DumpDir: "/proc/self/mem/nope"}
	rc := io.NopCloser(strings.NewReader("payload"))
	got := c.dumpUpstream(rc, []byte("{}"), &auth.Auth{UID: "u1"})
	body, err := io.ReadAll(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "payload" {
		t.Fatalf("落盘失败时仍必须原样透传，得到 %q", body)
	}
}

// ---------------------------------------------------------------------------
// 头族一致性 / 观测自愈（A1-18 / W4-U2-106 / A1-11）
// ---------------------------------------------------------------------------

// 调用方已给 traceparent 时，其余三条链路头仍必须补齐，且**沿用同一个 trace id**
// （审查 A1-18）。此前整族直接 return，透传场景下只剩一条 traceparent。
func TestInjectTraceHeadersFillsFamilyAroundCallerTraceparent(t *testing.T) {
	const callerTrace = "0af7651916cd43dd8448eb211c80319c"
	req, err := http.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-"+callerTrace+"-b7ad6b7169203331-01")
	injectTraceHeaders(req)

	if got := req.Header.Get("traceparent"); !strings.Contains(got, callerTrace) {
		t.Fatalf("调用方的 traceparent 不该被覆盖: %q", got)
	}
	b3 := req.Header.Get("b3")
	if b3 == "" {
		t.Fatal("b3 必须补齐（此前整族被 return 掉）")
	}
	if !strings.HasPrefix(b3, callerTrace+"-") {
		t.Fatalf("b3 必须沿用调用方的 trace id（否则链路断成两截）: %q", b3)
	}
	if req.Header.Get("X-B3-ParentSpanId") == "" {
		t.Fatal("X-B3-ParentSpanId 必须补齐")
	}
	if req.Header.Get("acp-connection-id") == "" {
		t.Fatal("acp-connection-id 必须补齐")
	}
}

// 调用方没给时四条都要生成（反向保护：不能为了 A1-18 把「本来就该生成」的路径改坏）。
func TestInjectTraceHeadersGeneratesWholeFamily(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	injectTraceHeaders(req)
	for _, h := range []string{"traceparent", "b3", "X-B3-ParentSpanId", "acp-connection-id"} {
		if req.Header.Get(h) == "" {
			t.Fatalf("%s 缺失", h)
		}
	}
	if id, ok := traceIDFromTraceparent(req.Header.Get("traceparent")); !ok || len(id) != 32 {
		t.Fatalf("生成的 traceparent 必须可解析: %q", req.Header.Get("traceparent"))
	}
	if !strings.HasPrefix(req.Header.Get("b3"), strings.Split(req.Header.Get("traceparent"), "-")[1]+"-") {
		t.Fatal("b3 与 traceparent 必须是同一个 trace id")
	}
}

// 404 冷却到期后必须重探（审查 W4-U2-106）：此前是进程级粘滞，上游补上端点后
// 繁忙度永远空白，且只有启动期一行日志。
func TestModelBusyReprobesAfterCooldown(t *testing.T) {
	g := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	newClient := func() (*busyStub, *Client) {
		stub := &busyStub{levels: map[string]string{}}
		c := stub.client()
		c.GlobalEnabled = true
		return stub, c
	}

	// 冷却期内：一次请求都不发。
	stub, c := newClient()
	c.modelBusyUnsupported.Store(true)
	c.modelBusyRetryAt.Store(time.Now().Add(time.Hour).UnixNano())
	if _, err := c.ModelBusy(g, []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	if len(stub.paths) != 0 {
		t.Fatalf("冷却期内不该探测，实际打了 %d 次", len(stub.paths))
	}

	// 冷却已过：重探一次，成功后解除跳过。
	stub2, c2 := newClient()
	c2.modelBusyUnsupported.Store(true)
	c2.modelBusyRetryAt.Store(time.Now().Add(-time.Minute).UnixNano())
	got, err := c2.ModelBusy(g, []string{"m1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub2.paths) != 1 {
		t.Fatalf("冷却到期必须重探一次，实际 %d 次", len(stub2.paths))
	}
	if got["m1"] != "normal" {
		t.Fatalf("重探结果没解析: %v", got)
	}
	if c2.modelBusyUnsupported.Load() {
		t.Fatal("探测成功后必须解除「不支持」标记（自愈闭环）")
	}
}

// *Error 的 HTTPStatus 必须填（审查 A1-11）：同一类错误两种口径会让排障误判。
func TestUpstreamErrorCarriesHTTPStatus(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(503, `{"code":10000,"msg":"boom"}`), nil
	})
	_, err := c.WebFetch(context.Background(), &auth.Auth{AccessToken: "at"}, "https://x.example")
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("应返回 *Error，得到 %T: %v", err, err)
	}
	if ue.HTTPStatus != 503 {
		t.Fatalf("HTTPStatus=%d want 503", ue.HTTPStatus)
	}
}
