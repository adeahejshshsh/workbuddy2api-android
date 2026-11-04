package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// 客户端掐断（Stop/掉线）不是故障：它以 context canceled 的形式散在多种文案里
// （transport error / upstream parse failed / 续写失败 / rotate backoff aborted），
// 线上实测 54 条聚合里有 37 条是它——一律归 info，免得把真故障挤出首屏。
func TestAggregateErrorsClientCancelIsInfo(t *testing.T) {
	ts := time.Date(2026, 10, 14, 1, 2, 3, 0, time.Local)
	lines := []LogLine{
		{Text: `ERR: [upstream] chat_stream acct=示例号(11f49997): transport error: Post "https://copilot.tencent.com/v2/chat/completions": context canceled`, TS: ts},
		{Text: `WARN: [server] sync acct=示例号(11f49997) model=deepseek-v4.1-flash: upstream parse failed: context canceled`, TS: ts},
		{Text: `WARN: [continue] acct=示例号(11f49997) seg=1 续写失败（status=0 err=Post "https://x": context canceled）→ 降级为截断终态`, TS: ts},
		{Text: `WARN: [server] rotate backoff aborted: ctx cancelled`, TS: ts},
		// 对照：真传输故障（不是掐断）仍须 bad
		{Text: `ERR: [upstream] chat_stream acct=备用号(aabbccdd): transport error: Post "https://x/v2/chat/completions": connection refused`, TS: ts},
	}
	got := AggregateErrors(lines)
	// 聚合键是 (账号, 类型, 模型)：示例号的掐断里有一条带 model，故与不带 model 的分列。
	byKey := map[string]ErrStat{}
	for _, r := range got {
		byKey[r.Account+"|"+r.Kind+"|"+r.Model] = r
	}
	if len(got) != 4 {
		t.Fatalf("聚合行数=%d want 4：%+v", len(got), got)
	}
	// 真故障（connection refused）仍须 bad，且排序上排最前
	if got[0].Kind != "传输失败" || got[0].Sev != sevBad {
		t.Errorf("真传输故障应仍是 bad 且排最前：%+v", got[0])
	}
	for _, k := range []string{
		"示例号(11f49997)|客户端中断|",
		"示例号(11f49997)|客户端中断|deepseek-v4.1-flash",
		"|客户端中断|",
	} {
		r, ok := byKey[k]
		if !ok {
			t.Fatalf("缺行 %q：%+v", k, got)
		}
		if r.Sev != sevInfo {
			t.Errorf("%q 的掐断应归 info：%+v", k, r)
		}
	}
	if r := byKey["示例号(11f49997)|客户端中断|"]; r.Count != 2 {
		t.Errorf("同账号同形掐断应合并计 2 次：%+v", r)
	}
}

// 缓存重算告警必须进汇总（线上 15 条 WARN 曾因无匹配规则被整类静默丢弃）。
func TestAggregateErrorsCacheRecompute(t *testing.T) {
	got := AggregateErrors([]LogLine{{
		Text: `WARN: [server] cache miss: model=deepseek-v4.1-flash 本次大前缀未命中（prompt=52508 miss=52508，此前观测到过命中）；上游前缀缓存重算，费用会显著升高`,
		TS:   time.Now(),
	}})
	if len(got) != 1 || got[0].Kind != "缓存重算（费用）" || got[0].Sev != sevWarn || got[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("缓存告警应归类为 warn：%+v", got)
	}
}

// 没匹配到规则的 WARN/ERR 归「其他（未归类）」，绝不静默丢弃：新增日志文案时要当场看见，
// 否则汇总看着正常、错的只是它少了一行（最难发现的漏报）。
func TestAggregateErrorsUnclassifiedNotDropped(t *testing.T) {
	got := AggregateErrors([]LogLine{
		{Text: `WARN: [brand-new] acct=示例号(11f49997) 某个还没写规则的失败: boom`, TS: time.Now()},
		{Text: `WARN: [brand-new] acct=备用号(aabbccdd) 另一个还没写规则的失败: boom2`, TS: time.Now()},
	})
	if len(got) != 2 {
		t.Fatalf("聚合行数=%d want 2（不同账号不并成一行）：%+v", len(got), got)
	}
	for _, r := range got {
		if r.Kind != "其他（未归类）" || r.Sev != sevWarn {
			t.Errorf("未归类行应归入「其他」且为 warn：%+v", r)
		}
		if r.Account == "" {
			t.Errorf("未归类行也要带得出账号：%+v", r)
		}
	}
	// 非 WARN/ERR 行仍不进汇总
	if n := len(AggregateErrors([]LogLine{{Text: `INFO: [x] 随便什么`, TS: time.Now()}})); n != 0 {
		t.Errorf("INFO 行不该进汇总：%d", n)
	}
}

// [continue] 行补上 acct= 之后，汇总里这一族（线上 22 条）必须带得出账号。
func TestAggregateErrorsContinueHasAccount(t *testing.T) {
	got := AggregateErrors([]LogLine{{
		Text: `WARN: [continue] acct=示例号(11f49997) 段无 finish 结束（疑似上游断流，按截断终态降级）`,
		TS:   time.Now(),
	}})
	if len(got) != 1 || got[0].Kind != "续写降级" || got[0].Account != "示例号(11f49997)" {
		t.Fatalf("续写降级应带账号：%+v", got)
	}
}

// 用本仓真实格式的样本行（取自 server/upstream/pool/continue 的实际 log.Printf 格式），
// 钉住三件事：归类正确、账号/模型/码抽取正确、按严重度+次数排序。
func TestAggregateErrors(t *testing.T) {
	ts := time.Date(2026, 10, 14, 1, 2, 3, 0, time.Local)
	lines := []LogLine{
		// 同账号同类型两行 → 合并计 2 次
		{Text: `WARN: [upstream] chat_stream acct=示例号(11f49997): upstream 429 soft_rate body={"code":6004,"msg":"rate limited"}`, TS: ts},
		{Text: `WARN: [upstream] chat_stream acct=示例号(11f49997): upstream 429 soft_rate body={"code":6004,"msg":"rate limited"}`, TS: ts.Add(time.Minute)},
		// 另一个账号的传输失败
		{Text: `ERR: [upstream] chat_stream acct=备用号(aabbccdd): transport error: Post "https://x/v2/chat/completions": connection refused`, TS: ts},
		// 非账号级：目录拉取
		{Text: `WARN: [upstream] fetch models: v3/config probe failed (degraded to enterprise endpoint): boom`, TS: ts},
		// 首帧否决（本仓新增流程）
		{Text: `WARN: [server] stream acct=示例号(11f49997) model=hy4-preview-f: upstream error frame before first byte kind=soft_rate payload={"error":{"message":"x"}} (rotating)`, TS: ts},
		// INFO 行与无法归类的行不得进聚合
		{Text: `INFO: [server] stream acct=示例号(11f49997) model=x: repaired 1 native tool call(s)`, TS: ts},
		{Text: `| #007 | 00:06:45 | cn:hy4-preview-f | sync | 200 | 示例号(11f49997) | TTFB=- |`, TS: ts},
	}
	got := AggregateErrors(lines)

	if len(got) != 4 {
		t.Fatalf("聚合行数=%d want 4：%+v", len(got), got)
	}
	// 排序：bad 在前（限流 2 次 > 传输 1 次），随后 warn（首帧否决、目录拉取按账号序）
	if got[0].Kind != "模型限流(6004)" || got[0].Count != 2 {
		t.Errorf("首行应为合并后的 6004 限流×2：%+v", got[0])
	}
	if got[0].Account != "示例号(11f49997)" || got[0].Code != "429" {
		t.Errorf("账号/上游状态码抽取错误：%+v", got[0])
	}
	if got[0].Last != "01:03:03" { // 取最近一次（第 2 行）
		t.Errorf("Last 应取最近一次时刻：%+v", got[0])
	}
	if got[1].Kind != "传输失败" || got[1].Sev != sevBad {
		t.Errorf("传输失败应归 bad：%+v", got[1])
	}
	// 两个 warn 各 1 次：账号序兜底（非账号级 "" 排在账号标签前）
	if got[2].Kind != "模型目录拉取异常" || got[2].Account != "" {
		t.Errorf("非账号级错误 Account 应为空：%+v", got[2])
	}
	if got[3].Kind != "首帧否决（换号）" || got[3].Model != "hy4-preview-f" {
		t.Errorf("首帧否决应带模型名：%+v", got[3])
	}
	for _, r := range got {
		if r.Sample == "" {
			t.Errorf("每行都应带样本：%+v", r)
		}
	}
}

// 调度族（scheduler）失败行：账号标签紧跟任务名、无 acct= 前缀，且连续 12153 禁用
// 是账号级终态（bad）而非普通任务失败。
func TestAggregateErrorsSchedulerFamily(t *testing.T) {
	ts := time.Now()
	got := AggregateErrors([]LogLine{
		{Text: `WARN: keepalive 示例号(11f49997): 连续 3 次 12153 session dead — 禁用`, TS: ts},
		{Text: `WARN: checkin 备用号(aabbccdd): Post "https://x": connection refused`, TS: ts},
		{Text: `WARN: balance 备用号(aabbccdd): context deadline exceeded`, TS: ts},
		{Text: `checkin 示例号(11f49997): 签到成功`, TS: ts}, // 正常流转无前缀 → 不参与
	})
	if len(got) != 3 {
		t.Fatalf("行数=%d want 3：%+v", len(got), got)
	}
	// bad 在前：保活连续 12153 禁用
	if got[0].Kind != "保活失败" || got[0].Sev != sevBad || got[0].Account != "示例号(11f49997)" {
		t.Errorf("first=%+v", got[0])
	}
	if got[1].Kind != "签到失败" || got[1].Account != "备用号(aabbccdd)" {
		t.Errorf("second=%+v", got[1])
	}
	if got[2].Kind != "余额查询失败" {
		t.Errorf("third=%+v", got[2])
	}
}

// 模型级限流的解除时刻从上游文案抽取（「将在 … 重置」/ 英文形态都认）。
func TestAggregateErrorsResetUntil(t *testing.T) {
	line := `WARN: [upstream] chat_stream acct=a1b2c3d4: upstream 429 soft_rate body={"code":6004,"msg":"rate limited, will reset at 2026-10-14 02:00:00"}`
	got := AggregateErrors([]LogLine{{Text: line, TS: time.Now()}})
	if len(got) != 1 || got[0].Until != "2026-10-14 02:00:00" {
		t.Fatalf("解除时刻未抽出：%+v", got)
	}
}

// 端点契约：前端读的是 {rows, scanned, updated_at}（renderErrorSummary 直接取这三项）。
func TestErrorSummaryEndpoint(t *testing.T) {
	p := New(Config{})
	p.Logs().Write([]byte("WARN: [upstream] chat_stream acct=a1b2c3d4: upstream 429 soft_rate body={\"code\":6004}\n"))
	rec := httptest.NewRecorder()
	p.errorSummary(rec, httptest.NewRequest("GET", "/panel/api/error_summary", nil))
	var got struct {
		Rows      []ErrStat `json:"rows"`
		Scanned   int       `json:"scanned"`
		UpdatedAt string    `json:"updated_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是预期 JSON：%v（body=%s）", err, rec.Body.String())
	}
	if got.Scanned != 1 || len(got.Rows) != 1 || got.Rows[0].Kind != "模型限流(6004)" || got.UpdatedAt == "" {
		t.Fatalf("端点输出不合契约：%+v", got)
	}
}

// 截断必须按 rune（中文样本不得切出半截 UTF-8）；且样本里换行被压平。
// 另：sample_full 必须比 sample 长且同样 rune 安全——表格正文与 title 悬停看全文
// 是两个字段（issue #61），title 复用已截断的 sample 会让悬停等于没有。
func TestAggregateErrorsSampleIsRuneSafe(t *testing.T) {
	line := "WARN: [upstream] chat_stream acct=a1b2c3d4: upstream 400 content_blocked body={\"msg\":\"" +
		strings.Repeat("您的使用量已超出频率限制，请稍后再试。", 20) + "\"}"
	got := AggregateErrors([]LogLine{{Text: line, TS: time.Now()}})
	if len(got) != 1 {
		t.Fatalf("应聚合成一行：%+v", got)
	}
	for i, r := range got[0].Sample {
		if r == '\uFFFD' {
			t.Fatalf("样本出现非法 UTF-8（第 %d 字节）：%q", i, got[0].Sample)
		}
	}
	if len(got[0].SampleFull) <= len(got[0].Sample) {
		t.Fatalf("sample_full 应比 sample 长（悬停才看得到全文）：sample=%d full=%d",
			len(got[0].Sample), len(got[0].SampleFull))
	}
	if len(got[0].SampleFull) > sampleFullBytes {
		t.Fatalf("sample_full 超过上限 %d：%d", sampleFullBytes, len(got[0].SampleFull))
	}
	for i, r := range got[0].SampleFull {
		if r == '\uFFFD' {
			t.Fatalf("全文样本出现非法 UTF-8（第 %d 字节）：%q", i, got[0].SampleFull)
		}
	}
}

// 错误汇总的「后端算好了、前端没渲染」是最容易悄悄发生的漂移：接口通了但页面
// 永远是空表，谁也不会发现。这里钉住三处接线——路由、表格容器、渲染函数。
func TestErrorSummaryWiring(t *testing.T) {
	src, err := os.ReadFile("panel.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"GET /panel/api/error_summary"`) {
		t.Error("panel.go 未注册 GET /panel/api/error_summary")
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{`id="errBody"`, `id="errNote"`} {
		if !strings.Contains(string(html), id) {
			t.Errorf("index.html 缺错误汇总容器 %s", id)
		}
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "function renderErrorSummary(") {
		t.Error("app.js 缺 renderErrorSummary")
	}
	if !strings.Contains(string(js), "api('error_summary')") {
		t.Error("app.js 没有拉取 error_summary")
	}
}
