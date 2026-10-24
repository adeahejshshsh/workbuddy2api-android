package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// expert_use_* 系列（专家页搜索并召唤 → 完成一次对话）的回归测试。
//
// 判据链与 Expert_lighthouse / WorkBuddy_Valorant 同源，但专家 id 各不相同、
// 且 market 关键词会命中多个同名专家（display_name_zh 与 profession_zh 都参与匹配）。
// 这里钉住三件事：登记完整性、按 expert_id 精确落位（不是取首条）、搜索失败时的静态兜底。

// TestExpertExperienceSpecsRegistered 登记表与 autoActions 双向一致。
// 只在一边登记就是静默故障：表里有码但 autoActions 没条目 → 任务中心不渲染按钮；
// autoActions 有条目但表里没参数 → 点击后 runExpertExperience 直接报"未登记"。
func TestExpertExperienceSpecsRegistered(t *testing.T) {
	if len(expertExperienceTasks) == 0 {
		t.Fatal("expertExperienceTasks 为空")
	}
	seen := map[string]string{}
	for code, spec := range expertExperienceTasks {
		act := autoActionFor(code)
		if act == nil {
			t.Errorf("expertExperienceTasks 有 %s、autoActions 缺（按钮点了会返回无动作）", code)
			continue
		}
		if act.run == nil {
			t.Errorf("%s 在 autoActions 里没有 run（永远跑不了）", code)
		}
		if strings.TrimSpace(act.Desc) == "" {
			t.Errorf("%s 缺 Desc", code)
		}
		if !strings.HasPrefix(spec.ExpertID, "ex_") {
			t.Errorf("%s 的 ExpertID=%q 不是市场真实 id 形态（自造 id 不计数）", code, spec.ExpertID)
		}
		if spec.ExpertType != "agent" {
			t.Errorf("%s 的 ExpertType=%q，want agent", code, spec.ExpertType)
		}
		if spec.DisplayNameZH == "" || spec.ProfessionZH == "" || spec.Version == "" {
			t.Errorf("%s 参数不全: %#v", code, spec)
		}
		if prev, dup := seen[spec.ExpertID]; dup {
			t.Errorf("%s 与 %s 用了同一个 ExpertID %s", code, prev, spec.ExpertID)
		}
		seen[spec.ExpertID] = code
	}
	// 反向：autoActions 里的 expert_use_* 必须都有参数表条目。
	for _, act := range autoActions {
		if !strings.HasPrefix(act.TaskCode, "expert_use_") {
			continue
		}
		if _, ok := expertExperienceTasks[act.TaskCode]; !ok {
			t.Errorf("autoActions 有 %s、expertExperienceTasks 缺参数（执行必报未登记）", act.TaskCode)
		}
	}
}

// expertFake 专家体验链的假上游：market 搜索 / chat SSE / report 三端点。
type expertFake struct {
	mu          sync.Mutex
	reported    []map[string]any
	searchBody  map[string]any
	marketFail  bool
	marketList  []upstream.MarketExpert
	searchCalls int
}

func (f *expertFake) events() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.reported))
	copy(out, f.reported)
	return out
}

func newExpertExperienceTestPanel(t *testing.T, f *expertFake) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/portal/operation-platform/market/expert/list":
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			f.mu.Lock()
			f.searchCalls++
			f.searchBody = body
			fail, list := f.marketFail, f.marketList
			f.mu.Unlock()
			if fail {
				http.Error(w, "market unavailable", http.StatusBadGateway)
				return
			}
			out, _ := json.Marshal(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"total_count": len(list), "experts": list},
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case r.URL.Path == "/v2/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[]}\n\n")
		case r.URL.Path == "/v2/report":
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			var evs []map[string]any
			_ = json.Unmarshal(raw, &evs)
			f.mu.Lock()
			f.reported = append(f.reported, evs...)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":0,"msg":"OK","data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := upstream.New()
	client.ChatBaseCN, client.WebBaseCN, client.BillingBaseCN = srv.URL, srv.URL, srv.URL
	p := New(Config{Upstream: client})
	return p, &auth.Auth{UID: "expert-experience-test", AccessToken: "token"}
}

// findEvent 在已上报事件里找指定 eventCode 的第一条。
func findEvent(t *testing.T, events []map[string]any, code string) map[string]any {
	t.Helper()
	for _, ev := range events {
		if ev["eventCode"] == code {
			return ev
		}
	}
	t.Fatalf("没有上报 %s 事件（共 %d 条）", code, len(events))
	return nil
}

// TestRunExpertExperiencePicksExpertByID 关键词命中多个同名专家时必须按 id 落位。
// 假市场把「同名的另一个专家」放在首条、把真实条目放在第二条且 version 与静态表不同：
// 取首条会打错专家（不计数），不读市场则 version 用的是表内旧值。
func TestRunExpertExperiencePicksExpertByID(t *testing.T) {
	const code = "expert_use_douyinyin"
	spec := expertExperienceTasks[code]
	f := &expertFake{marketList: []upstream.MarketExpert{
		{ExpertID: "ex_WmcyDlsTPaiS", ExpertType: "agent", DisplayNameZH: "抖音策略师",
			ProfessionZH: "抖音全域增长策略师", Version: "1.0.0"},
		{ExpertID: spec.ExpertID, ExpertType: "agent", DisplayNameZH: "斗音音",
			ProfessionZH: "抖音策略师", Version: "9.9.9"},
	}}
	p, a := newExpertExperienceTestPanel(t, f)

	if _, err := p.runExpertExperience(a, code); err != nil {
		t.Fatalf("runExpertExperience: %v", err)
	}
	if f.searchBody["keyword"] != spec.ProfessionZH {
		t.Fatalf("搜索关键词 = %v, want %q", f.searchBody["keyword"], spec.ProfessionZH)
	}
	events := f.events()
	summon := findEvent(t, events, "expert_summon_click")
	if summon["id"] != spec.ExpertID {
		t.Fatalf("召唤的专家 = %v, want %s", summon["id"], spec.ExpertID)
	}
	if summon["version"] != "9.9.9" {
		t.Fatalf("召唤事件 version = %v, want 市场值 9.9.9（说明没读市场）", summon["version"])
	}
	created := findEvent(t, events, "agent_task_created")
	if created["has_expert"] != true || created["expert_id"] != spec.ExpertID {
		t.Fatalf("agent_task_created 未带专家: has_expert=%v expert_id=%v", created["has_expert"], created["expert_id"])
	}
	use := findEvent(t, events, "expert_actual_use")
	if use["id"] != spec.ExpertID || use["name"] != "斗音音" {
		t.Fatalf("expert_actual_use 专家 = %v/%v, want %s/斗音音", use["id"], use["name"], spec.ExpertID)
	}
	if use["mode"] != "LOCAL" {
		t.Fatalf("expert_actual_use mode = %v, want LOCAL", use["mode"])
	}
	// 对齐真实样本：专家页直连召唤的 actual_use type 为空、cost=0。
	if use["type"] != "" || fmt.Sprint(use["cost"]) != "0" {
		t.Fatalf("expert_actual_use type/cost = %v/%v, want \"\"/0", use["type"], use["cost"])
	}
	if use["requestId"] != "cmb-0123456789abcdef0123456789abcdef" {
		t.Fatalf("expert_actual_use requestId = %v（必须是服务端真实 id）", use["requestId"])
	}
}

// TestRunExpertExperienceFallsBackToStaticSpec 市场不可用时仍要能上报（召唤链不依赖搜索）。
func TestRunExpertExperienceFallsBackToStaticSpec(t *testing.T) {
	const code = "expert_use_maidehao"
	spec := expertExperienceTasks[code]
	f := &expertFake{marketFail: true}
	p, a := newExpertExperienceTestPanel(t, f)

	if _, err := p.runExpertExperience(a, code); err != nil {
		t.Fatalf("runExpertExperience: %v", err)
	}
	if f.searchCalls != 1 {
		t.Fatalf("market 搜索调用 = %d, want 1", f.searchCalls)
	}
	summon := findEvent(t, f.events(), "expert_summon_click")
	if summon["id"] != spec.ExpertID || summon["version"] != spec.Version {
		t.Fatalf("兜底参数 = %v/%v, want %s/%s", summon["id"], summon["version"], spec.ExpertID, spec.Version)
	}
}

// TestRunExpertExperienceUnknownCode 未登记任务码必须报错而不是静默成功。
func TestRunExpertExperienceUnknownCode(t *testing.T) {
	p, a := newExpertExperienceTestPanel(t, &expertFake{})
	if _, err := p.runExpertExperience(a, "expert_use_not_registered"); err == nil {
		t.Fatal("未登记的任务码应当报错")
	}
}
