package panel

// 「该账号没有此任务」的判据必须建立在**两份**任务清单的并集上（上游 0a2a55f）。
//
// 任务清单来自两处：默认口径（无 X-Client-Platform 头）与 mp 口径。此前代码假设
// 「mp 列表是默认口径的超集」，判定只信一路；任务中心改版后 mp 口径成了子集，默认
// 清单里独有的任务（issue #243 的 Library_read）被整批判成「该账号没有此任务」，
// 一项都不做。下面钉住三件事：
//  1. 合并去重的顺序稳定（面板每次刷新按这个顺序渲染，不能乱跳）；
//  2. 单任务回读默认清单未命中时回落 mp 口径；
//  3. 一键完成不再把只存在于另一份清单的任务判成缺失（两个方向都测）。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// splitListFake 假上游：默认口径与 mp 口径各下发自己的任务码，其余请求一律 200 空信封。
// seen 记录每次列表请求走了哪条口径（path + " mp"），供断言「回落真的问了 mp」。
type splitListFake struct {
	codes   []string // 默认口径（无 mp 头）下发的码
	mpCodes []string // mp 口径下发的码
	mu      sync.Mutex
	seen    []string
}

func (f *splitListFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 按**取值**认口径，不能按「有这个头」认（issue #257）：CN 桌面端在 billing /
		// growth 族上也会发 X-Client-Platform: windows，存在性判据会把 CN 请求当成 mp。
		mp := r.Header.Get("X-Client-Platform") == "miniprogram"
		f.mu.Lock()
		tag := r.URL.Path
		if mp {
			tag += " mp"
		}
		f.seen = append(f.seen, tag)
		codes := f.codes
		if mp {
			codes = f.mpCodes
		}
		f.mu.Unlock()

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/growth/tasks") {
			tasks := make([]any, 0, len(codes))
			for _, c := range codes {
				tasks = append(tasks, map[string]any{"task_code": c, "current": 0, "target": 1})
			}
			body, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK", "data": map[string]any{"tasks": tasks},
			})
			_, _ = w.Write(body)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"msg":"OK","data":{}}`)
	}
}

// splitListCalls 返回列表请求的口径序列（"cn" / "mp"）。
func (f *splitListFake) splitListCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.seen {
		if !strings.Contains(s, "/growth/tasks") || strings.Contains(s, "/accept") {
			continue
		}
		if strings.HasSuffix(s, " mp") {
			out = append(out, "mp")
			continue
		}
		out = append(out, "cn")
	}
	return out
}

// splitListPanel 装配指向 fake 上游的面板（三域同源部署，按头分流）。
func splitListPanel(t *testing.T, f *splitListFake) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	pl := pool.New("")
	a := &auth.Auth{UID: "cn-1", Nickname: "cn", AccessToken: "at", ExpiresAt: 9999999999}
	pl.Add(a)
	p := New(Config{Pool: pl, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}})
	return p, a
}

// TestMergeTaskListsDedupesKeepsOrder 合并去重：默认清单顺序原样保留，只存在于 mp
// 清单的任务按 mp 原顺序追加到尾部；同名（忽略大小写）只留第一条；空 code 丢弃。
func TestMergeTaskListsDedupesKeepsOrder(t *testing.T) {
	base := []upstream.Task{{TaskCode: "Library_read"}, {TaskCode: "chat_5"}}
	extra := []upstream.Task{
		{TaskCode: "library_READ"}, // 与 base 首条同名（大小写不同）→ 去重
		{TaskCode: "Sequential_Tasks_1"},
		{TaskCode: ""},       // 占位项 → 丢弃
		{TaskCode: "Chat_5"}, // 与 base 次条同名 → 去重
		{TaskCode: "school_season"},
	}
	want := []string{"Library_read", "chat_5", "Sequential_Tasks_1", "school_season"}

	got := mergeTaskLists(base, extra)
	if len(got) != len(want) {
		t.Fatalf("合并结果 %d 条，want %d：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].TaskCode != want[i] {
			t.Fatalf("第 %d 条 = %q，want %q（顺序必须稳定）", i, got[i].TaskCode, want[i])
		}
	}
	// 同一份输入连跑多次必须完全一致：合并不能依赖 map 迭代序。
	for n := 0; n < 20; n++ {
		again := mergeTaskLists(base, extra)
		for i := range again {
			if again[i].TaskCode != got[i].TaskCode {
				t.Fatalf("第 %d 次合并顺序变了：%q != %q", n, again[i].TaskCode, got[i].TaskCode)
			}
		}
	}
}

// TestTaskByCodeFallsBackToMPList 单任务回读：默认清单未命中要再问 mp 口径。
// Library_read 没登记进 mpTaskCodes —— 回落不能依赖那张登记表，否则常规任务只要
// 只在 mp 清单下发就永远被判「该账号没有此任务」。
func TestTaskByCodeFallsBackToMPList(t *testing.T) {
	if isMPTaskCode("Library_read") {
		t.Fatal("测试前置：Library_read 不是登记过的 mp 专属码，本用例才有效")
	}
	f := &splitListFake{codes: []string{"chat_5"}, mpCodes: []string{"Library_read"}}
	p, a := splitListPanel(t, f)

	got, err := p.taskByCode(a, "Library_read")
	if err != nil {
		t.Fatalf("taskByCode: %v", err)
	}
	if got == nil {
		t.Fatalf("默认清单未命中时应回落 mp 口径找到该任务，实际 nil；列表请求=%v", f.splitListCalls())
	}
	if got.TaskCode != "Library_read" {
		t.Fatalf("回落拿到的任务码 = %q，want Library_read", got.TaskCode)
	}
	var sawMP bool
	for _, c := range f.splitListCalls() {
		if c == "mp" {
			sawMP = true
		}
	}
	if !sawMP {
		t.Fatalf("没有向 mp 口径回落，列表请求=%v", f.splitListCalls())
	}
}

// TestLoadTaskMapMergesBothLists 快照（一键完成用的任务映射）必须是两份清单的并集：
// 默认清单独有（Library_read，issue #243 的形态）与 mp 清单独有的码都要在里面。
func TestLoadTaskMapMergesBothLists(t *testing.T) {
	f := &splitListFake{codes: []string{"Library_read"}, mpCodes: []string{"Sequential_Tasks_1"}}
	p, a := splitListPanel(t, f)

	m, err := p.loadTaskMap(a)
	if err != nil {
		t.Fatalf("loadTaskMap: %v", err)
	}
	if m == nil {
		t.Fatal("loadTaskMap 返回 nil")
	}
	if m["library_read"] == nil {
		t.Fatalf("只在默认清单里的任务丢了：%v", m)
	}
	if m["sequential_tasks_1"] == nil {
		t.Fatalf("只在 mp 清单里的任务丢了：%v", m)
	}
}

// TestRunAutoAllKeepsTasksOnlyInOneList 一键完成：只存在于任一份清单的任务都不能被
// 判成「该账号没有此任务」，而且都要真的执行。
//
// 两个方向都覆盖：chat_5 只在默认清单（issue #243 的形态：mp 成了子集），
// Library_read 只在 mp 清单（另一路的对称形态）。
func TestRunAutoAllKeepsTasksOnlyInOneList(t *testing.T) {
	oldGap, oldPoll := reportGap, claimPollGap
	reportGap, claimPollGap = 0, 0
	t.Cleanup(func() { reportGap, claimPollGap = oldGap, oldPoll })

	f := &splitListFake{codes: []string{"chat_5"}, mpCodes: []string{"Library_read"}}
	p, a := splitListPanel(t, f)

	res := p.runAutoAllWith(a, nil)
	rows := map[string]map[string]any{}
	for _, r := range res {
		if code, ok := r["task_code"].(string); ok {
			rows[code] = r
		}
	}

	for _, code := range []string{"chat_5", "Library_read"} {
		row := rows[code]
		if row == nil {
			t.Fatalf("结果里没有 %s 这一项：%+v", code, res)
		}
		if row["message"] == "该账号无此任务" {
			t.Fatalf("%s 存在于另一份清单，却被判「该账号无此任务」（两路清单没有合并）: %+v", code, row)
		}
		if row["status"] != "done" {
			t.Fatalf("%s 应当被执行（status=done），实际 %+v", code, row)
		}
	}
}
