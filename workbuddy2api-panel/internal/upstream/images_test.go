package upstream

import "testing"

// TestNonChatModelAndGenerationKind 非对话模型判定必须覆盖媒体生成标签：
// 出图（text-to-image / image-to-image）与出视频（text-to-video / image-to-video）。
// 视频这两条以前漏了 —— seedance-2.5 因此被当成对话模型列进 global 名单，面板里还
// 显示成"不支持思考"；客户端选它发对话只会拿到 11102/14407。
func TestNonChatModelAndGenerationKind(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		maxOut   int64
		tags     []string
		nonChat  bool
		kindWant string
	}{
		{"出图", "hunyuan-image-alpha", 0, []string{"text-to-image"}, true, "image"},
		{"改图", "hunyuan-image-alpha-edit", 0, []string{"image-to-image"}, true, "image"},
		{"出视频", "seedance-2.5", 0, []string{"text-to-video", "image-to-video"}, true, "video"},
		{"改视频（大写/空白容错）", "x", 0, []string{" Text-To-Video "}, true, "video"},
		{"普通对话模型", "glm-5.2", 32000, []string{"craft"}, false, ""},
		{"craft + 视频标签仍算出视频", "y", 32000, []string{"craft", "text-to-video"}, true, "video"},
		{"前缀过滤仍生效", "codewise-jump", 0, nil, true, ""},
		{"tiny 输出仍过滤", "z", 256, nil, true, ""},
	}
	for _, c := range cases {
		if got := nonChatModel(c.id, c.maxOut, c.tags); got != c.nonChat {
			t.Errorf("%s: nonChatModel=%v want %v", c.name, got, c.nonChat)
		}
		if got := generationKind(c.tags); got != c.kindWant {
			t.Errorf("%s: generationKind=%q want %q", c.name, got, c.kindWant)
		}
	}
}

// TestStashGenerationModelsMergesPerID 留存按 ID 合并而非覆盖：global 目录是多 UA 并发
// 探测的，宽/窄两份目录都会落一笔——覆盖式留存会让结果取决于谁先返回（实测表现为
// 「这次有 hy-image-3.5、下次变成 hunyuan-image-alpha」，seedance-2.5 时有时无）。
//
// 另：hunyuan-image-alpha 虽可能被 global 目录列出，但国际侧**没有出图路由**（实测
// 400 code=14401 route config not found），故列表侧按 imageRouteRealm 过滤掉——这正是
// 「模型列出来却必失败」的修复点，故 global 侧同样只剩 2 条。
func TestStashGenerationModelsMergesPerID(t *testing.T) {
	c := &Client{}
	c.stashGenerationModels("global", []ModelInfo{
		{ID: "hy-image-3.5", Tags: []string{"text-to-image"}},
	})
	c.stashGenerationModels("global", []ModelInfo{
		{ID: "hunyuan-image-alpha", Tags: []string{"text-to-image"}},
		{ID: "seedance-2.5", Tags: []string{"text-to-video", "image-to-video"}},
		{ID: "glm-5.2", Tags: []string{"craft"}}, // 非媒体模型不入留存
	})
	got := c.GenerationModels("global")
	if len(got) != 2 {
		t.Fatalf("global 留存应合并成 2 条（窄目录不能顶掉宽目录；hunyuan 无本域路由须剔除），实际 %d：%+v", len(got), got)
	}
	kinds := map[string]string{}
	for _, m := range got {
		kinds[m.ID] = m.GenerationKind
	}
	if kinds["hy-image-3.5"] != "image" || kinds["seedance-2.5"] != "video" {
		t.Fatalf("kind 标记不对：%+v", kinds)
	}
	if _, bad := kinds["hunyuan-image-alpha"]; bad {
		t.Fatalf("国际侧无路由的 hunyuan-image-alpha 不得出现在 global 列表：%+v", got)
	}
	// 顺序稳定（按 ID 排序），重复留存不抖
	again := c.GenerationModels("global")
	for i := range got {
		if got[i].ID != again[i].ID {
			t.Fatalf("留存顺序不稳定：%v vs %v", got, again)
		}
	}
}

// TestGenerationModelsRouteFilter 列表只列本域真有路由的图像模型：
// 目录里出现 ≠ 本域能调。实测（2026-10-22 两个个人号直连上游）：
//   - hunyuan-image-alpha 只有国内路由；hy-image-3.5 只有国际路由；
//   - gpt-image-2.5-sunburst 已下架（14401 route config not found），两域都不列；
//   - 未知模型不在表里，照旧列出（不猜、不隐藏未实测的能力）。
func TestGenerationModelsRouteFilter(t *testing.T) {
	c := &Client{}
	for _, realm := range []string{"cn", "global"} {
		c.stashGenerationModels(realm, []ModelInfo{
			{ID: "hunyuan-image-alpha", Tags: []string{"text-to-image"}},
			{ID: "hy-image-3.5", Tags: []string{"text-to-image"}},
			{ID: "gpt-image-2.5-sunburst", Tags: []string{"text-to-image"}}, // 已下架
			{ID: "brand-new-image-v9", Tags: []string{"text-to-image"}},     // 表里没有 → 照列
		})
	}
	ids := func(realm string) map[string]bool {
		out := map[string]bool{}
		for _, m := range c.GenerationModels(realm) {
			out[m.ID] = true
		}
		return out
	}
	cn, gl := ids("cn"), ids("global")
	if !cn["hunyuan-image-alpha"] || cn["hy-image-3.5"] || cn["gpt-image-2.5-sunburst"] {
		t.Fatalf("cn 列表应含 hunyuan、不含 hy-image-3.5 与已下架的 sunburst：%v", cn)
	}
	if !gl["hy-image-3.5"] || gl["hunyuan-image-alpha"] || gl["gpt-image-2.5-sunburst"] {
		t.Fatalf("global 列表应含 hy-image-3.5、不含 hunyuan 与已下架的 sunburst：%v", gl)
	}
	if !cn["brand-new-image-v9"] || !gl["brand-new-image-v9"] {
		t.Fatalf("未实测的模型应照旧列出（不隐藏能力）：cn=%v global=%v", cn, gl)
	}
}
