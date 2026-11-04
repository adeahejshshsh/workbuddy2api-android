package pool

import (
	"os"
	"strings"
	"testing"
)

func TestClassifyPlan(t *testing.T) {
	cases := []struct {
		name string
		pkgs []string
		want PlanKind
	}{
		{"无包信息不猜", nil, PlanUnknown},
		{"空列表不猜", []string{}, PlanUnknown},
		{"免费版", []string{"Free Plan"}, PlanFree},
		{"免费版中文", []string{"免费版"}, PlanFree},
		{"体验版", []string{"CodeBuddy 体验版"}, PlanTrial},
		{"标准版", []string{"标准版"}, PlanStandard},
		{"订阅优先于活动包", []string{"国内运营裂变包", "标准版"}, PlanStandard},
		{"全活动包", []string{"国内运营裂变包", "拉新权益包"}, PlanActivity},
		{"混合来源不敢判", []string{"国内运营裂变包", "某未知来源包"}, PlanUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ev := ClassifyPlan(c.pkgs)
			if got != c.want {
				t.Errorf("ClassifyPlan(%v)=%s（证据 %q）want %s", c.pkgs, got, ev, c.want)
			}
			if got != PlanUnknown && ev == "" {
				t.Errorf("非 unknown 必须给出命中证据，got %s/%q", got, ev)
			}
		})
	}
}

// 分类算得再准，没人调也是死的。这条钉住接线：/panel/api/packages 的每行必须带
// plan 字段，且前端确实把它渲染出来——「后端算好了、前端没渲染」是最容易悄悄发生
// 的漂移（接口通了但页面上永远看不到）。
func TestPlanWiredIntoPackages(t *testing.T) {
	src, err := os.ReadFile("../panel/panel.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "pool.ClassifyPlan(names)") {
		t.Error("panel.go 的 packages 没有调用 pool.ClassifyPlan")
	}
	if !strings.Contains(string(src), `json:"plan"`) {
		t.Error("packages 行结构缺 plan 字段")
	}
	js, err := os.ReadFile("../panel/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "PLAN_LABELS") {
		t.Error("app.js 缺 PLAN_LABELS（套餐分类没有对应文案）")
	}
	if !strings.Contains(string(js), "PLAN_LABELS[a.plan]") {
		t.Error("app.js 没有渲染 a.plan")
	}
}
