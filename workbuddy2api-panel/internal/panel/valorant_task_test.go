package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// WorkBuddy_Valorant 是「指定某一个专家」的体验任务（+100c+5e，截止 11-15），
// 与 expert_5（任意 5 个专家）不同：判据要求该域市场里真实存在的那一个专家。
func TestValorantActionRegistered(t *testing.T) {
	act := autoActionFor("WorkBuddy_Valorant")
	if act == nil {
		t.Fatal("WorkBuddy_Valorant 必须出现在 autoActions 里（否则面板「一键完成」点不动它）")
	}
	if act.Attempt {
		t.Error("这是已验证可点亮的任务，不应标成尝试型（否则默认不执行）")
	}
	if act.run == nil {
		t.Error("run 不能为空")
	}
}

// 国服/国际服是两套专家市场：国服有已知 id，国际服靠名称匹配。
func TestValorantExpertIDPerRealm(t *testing.T) {
	if got := valorantExpertIDs["cn"]; got != "ex_xQHHD1u5Jkua" {
		t.Errorf("国服专家 id = %q, want ex_xQHHD1u5Jkua", got)
	}
	// 国际服未实测到独立 id：留空表示"只能按名称匹配"，绝不复用国服 id。
	if got := valorantExpertIDs["global"]; got != "" {
		t.Errorf("国际服 id 未验证前必须留空（当前 %q）——拿国服 id 打国际服只会白跑", got)
	}
}

func TestMatchValorantExpert(t *testing.T) {
	cnList := []upstream.MarketExpert{
		{ExpertID: "ex_other", DisplayNameZH: "文档助手", ProfessionZH: "办公"},
		{ExpertID: "ex_xQHHD1u5Jkua", DisplayNameZH: "无畏契约拼豆专家", ProfessionZH: "游戏设计"},
	}
	if e, ok := matchValorantExpert(cnList, "ex_xQHHD1u5Jkua"); !ok || e.ExpertID != "ex_xQHHD1u5Jkua" {
		t.Fatalf("按 id 精确命中失败: %+v ok=%v", e, ok)
	}

	// 国际服 id 不同（ex_zzz），但名称是英文：按关键词兜底命中。
	globalList := []upstream.MarketExpert{
		{ExpertID: "ex_other", DisplayNameZH: "Doc Helper"},
		{ExpertID: "ex_zzz", DisplayNameZH: "Valorant Perler Expert", ProfessionZH: "Game Design"},
	}
	if e, ok := matchValorantExpert(globalList, ""); !ok || e.ExpertID != "ex_zzz" {
		t.Fatalf("按名称兜底命中失败: %+v ok=%v", e, ok)
	}

	// 中文名 + 不同 id 也要能命中。
	zhList := []upstream.MarketExpert{{ExpertID: "ex_abc", DisplayNameZH: "拼豆工坊"}}
	if e, ok := matchValorantExpert(zhList, ""); !ok || e.ExpertID != "ex_abc" {
		t.Fatalf("中文关键词命中失败: %+v ok=%v", e, ok)
	}

	// 都没有 → 必须明确返回 false（调用方据此报错，不拿错 id 白跑）。
	if _, ok := matchValorantExpert([]upstream.MarketExpert{{ExpertID: "ex_x", DisplayNameZH: "无关专家"}}, "ex_xQHHD1u5Jkua"); ok {
		t.Fatal("不相关的列表不应命中")
	}
}
