// clientpick_test.go 「/v1/models 与官方客户端菜单一致」的回归钉。
//
// 上游形状（2026-10-29 真机 5.9.3 两域 /v3/config 实测）：
//
//	CN    data.models 58 条 / data.agents[cli].models 16 条 / 客户端菜单 17 项
//	INTL  data.models 25 条 / data.agents[cli].models 23 条 / 客户端菜单 22 项
//
// 差距的来源就是本文件钉住的三条规则（详见 clientpick.go 头部）：paidId 不单列、
// auto 是开关不是菜单行、速度预设即使不在 agent 名单里也显示。写反任何一条，
// 用户侧都会看到「模型列表和官方客户端对不上」——而这正是本次改动的动因。
package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cnV3ConfigFixture 按 5.9.3 CN 真机响应裁剪出的最小样本，保留每个关键形态：
// 速度预设（不在 agents 里）、auto（在 agents 里但不是菜单行）、paidId（hy3-x）、
// 路由变体（hy3-b）、已 disabled（wb-lite）、补全模型（codewise-*）。
const cnV3ConfigFixture = `{"code":0,"data":{
 "models":[
  {"id":"fast-model","name":"快速","maxInputTokens":200000,"maxOutputTokens":48000},
  {"id":"balanced-model","name":"均衡","maxInputTokens":256000,"maxOutputTokens":48000},
  {"id":"deep-model","name":"极致","maxInputTokens":272000,"maxOutputTokens":48000},
  {"id":"auto","name":"Auto","maxInputTokens":200000,"maxOutputTokens":32000},
  {"id":"hy3","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high"]}},
  {"id":"hy3-b","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000},
  {"id":"hy3-x","name":"Hy3","maxInputTokens":192000,"maxOutputTokens":64000},
  {"id":"deepseek-v4.1-flash","name":"DeepSeek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000},
  {"id":"glm-5.3-flash","name":"GLM-5.3-Flash","maxInputTokens":1000000,"maxOutputTokens":131072},
  {"id":"codewise-jump","name":"codewise-jump","maxInputTokens":8000,"maxOutputTokens":128},
  {"id":"wb-lite","name":"wb-lite","disabled":true}
 ],
 "agents":[{"name":"cli","tags":["cli","default","model:craft"],
   "models":["auto","hy3","hy3-x","deepseek-v4.1-flash","glm-5.3-flash"]}],
 "productFeaturesConfig":{"ModelRateLimitCap":{"lines":[
   {"displayName":"Hy3","freeId":"hy3","paidId":"hy3-x","allowPaidSwitch":true}]}}
}}`

// cnMenuWant 客户端模型菜单的真实内容与顺序（真机 DOM 抓的 17 项里的这一段）：
// 三个速度预设永远在最前，其余按 agents.models 原序；auto 与 hy3-x 不出现。
var cnMenuWant = []string{"fast-model", "balanced-model", "deep-model",
	"hy3", "deepseek-v4.1-flash", "glm-5.3-flash"}

// TestFetchV3ModelsMatchesClientMenu CN 域：目录里多出来的 hy3-b（变体）、auto（开关）、
// hy3-x（paidId）、codewise-jump（补全）、wb-lite（disabled）都不许进对话列表。
//
// 反向代价：这条断言松掉，就等于允许「上游目录有就列」——那正是本次修掉的 bug，
// 下游客户端会选到 11102 或客户端根本不显示的 id。
func TestFetchV3ModelsMatchesClientMenu(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v3/config") {
			t.Errorf("unexpected path %s", r.URL.Path)
			return jsonResp(404, `{}`), nil
		}
		return jsonResp(200, cnV3ConfigFixture), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	infos, err := c.fetchV3Models(a)
	if err != nil {
		t.Fatalf("fetchV3Models: %v", err)
	}
	got := make([]string, 0, len(infos))
	for _, mi := range infos {
		got = append(got, mi.ID)
	}
	if strings.Join(got, ",") != strings.Join(cnMenuWant, ",") {
		t.Fatalf("menu order/ids = %v want %v", got, cnMenuWant)
	}
}

// TestClientPickDropsPaidRateLimitVariant INTL 域：deepseek-v4.1-flash-sg 是
// deepseek-v4.1-flash 的付费变体（ModelRateLimitCap.paidId），客户端把它并进免费那一行，
// 不单独列。若把它当独立模型列给下游，用户会以为有两个不同模型。
func TestClientPickDropsPaidRateLimitVariant(t *testing.T) {
	byID := map[string]ModelInfo{
		"deepseek-v4.1-flash":    {ID: "deepseek-v4.1-flash"},
		"deepseek-v4.1-flash-sg": {ID: "deepseek-v4.1-flash-sg"},
		"kimi-k3":                {ID: "kimi-k3"},
	}
	pick := newClientPick(
		[]v3ConfigAgent{{Name: "cli", Tags: []string{"default"},
			Models: []string{"deepseek-v4.1-flash", "deepseek-v4.1-flash-sg", "kimi-k3"}}},
		[]v3RateLimitLine{{FreeID: "deepseek-v4.1-flash", PaidID: "deepseek-v4.1-flash-sg"}},
	)
	got := pick.ordered(byID)
	want := []string{"deepseek-v4.1-flash", "kimi-k3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ordered = %v want %v", got, want)
	}
}

// TestClientPickWithoutAgentsKeepsFullCatalog 口径缺失不裁剪：企业端点 / 私有化 / 老版本
// 不下发 agents 时无从得知客户端菜单，此时必须照旧全量列出，而不是返回空列表。
// 反向代价：这里若改成「无 agents 就全隐藏」，所有非 SaaS 部署的 /v1/models 会直接空掉。
func TestClientPickWithoutAgentsKeepsFullCatalog(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"glm-5.2","name":"GLM-5.2","maxOutputTokens":64000},
			{"id":"hy3-b","name":"Hy3","maxOutputTokens":64000}
		]}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	infos, err := c.fetchV3Models(a)
	if err != nil {
		t.Fatalf("fetchV3Models: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("infos=%v want 2 entries (no agents → keep full catalog)", idsOf(infos))
	}
}

// TestClientPickPrefersDefaultTaggedAgent 客户端选默认 agent 的优先级是
// tags 含 default → name==cli。若先按 name 选，企业部署里 cli agent 的名单会
// 覆盖掉真正给菜单用的那个 agent。
func TestClientPickPrefersDefaultTaggedAgent(t *testing.T) {
	pick := newClientPick([]v3ConfigAgent{
		{Name: "cli", Models: []string{"m-cli-only"}},
		{Name: "desktop", Tags: []string{"default"}, Models: []string{"m-default"}},
	}, nil)
	byID := map[string]ModelInfo{"m-cli-only": {ID: "m-cli-only"}, "m-default": {ID: "m-default"}}
	// 三个速度预设不在目录里，按 ordered 的规则跳过，只留下 default agent 的那条。
	got := pick.ordered(byID)
	if strings.Join(got, ",") != "m-default" {
		t.Fatalf("ordered = %v want [m-default]", got)
	}
}

// TestGenerationModelsSurviveVisibilityFilter 媒体模型不在 agent 菜单里，但仍要进
// GenerationModels 单列（/v1/models 按 image_generation 标记列出）。
// 若把 stash 挪到可见性过滤之后，出图模型会凭空消失。
func idsOf(infos []ModelInfo) []string {
	out := make([]string, 0, len(infos))
	for _, mi := range infos {
		out = append(out, mi.ID)
	}
	return out
}

// TestClientPickIntlMenuOrder INTL 域顺序钉：客户端菜单首项是 default-model（Auto），
// 而 fast-model / balanced-model 这段**在 agents 名单里**，不能像 CN 那样前置。
//
// 这两条真机菜单顺序不同（CN 预设在前、INTL 纯 agent 序），故 ordered 的规则是
// 「只前置不在 agent 名单里的预设」。曾写成「预设一律前置」：id 集合仍对、首项变成
// Fast，与客户端菜单不符。
func TestClientPickIntlMenuOrder(t *testing.T) {
	byID := map[string]ModelInfo{}
	for _, id := range []string{"default-model", "fast-model", "balanced-model", "primary-model", "hy3"} {
		byID[id] = ModelInfo{ID: id}
	}
	pick := newClientPick([]v3ConfigAgent{{Name: "cli", Tags: []string{"default"},
		Models: []string{"default-model", "fast-model", "balanced-model", "primary-model", "hy3"}}}, nil)
	got := pick.ordered(byID)
	want := []string{"default-model", "fast-model", "balanced-model", "primary-model", "hy3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ordered = %v want %v", got, want)
	}
}
