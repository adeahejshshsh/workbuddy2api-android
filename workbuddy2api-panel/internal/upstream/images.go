// images.go 文生图 / 图生图：POST {chatBase}/v2/images/generations 与 /v2/images/edits。
//
// 实测（带池内真实 token）：上游只有这两条图像路由——/v1/images/generations、
// /v2/image/generations、/v2/plugin/images/generations、/v2/chat/images/generations 全
// `{"error_msg":"404 Route Not Found"}`。请求 {model,prompt,n,size}：size 生效（默认
// 1536x1536）、n 被忽略（恒 1 张）、未知键被忽略；回程信封 data 已是 OpenAI 形状
// {created,data:[{url}],usage{...,credit}}，图是签名 COS 直链（约 24h 有效）——网关只拆
// 信封，不转存字节。edits 多一个 image（data URL 数组）。
//
// 模型名按 realm 不同，**计费模型也完全不同**（2026-10-22 复测，两个个人号直连上游）：
//
//	cn     hunyuan-image-alpha   每张固定 5.71 credit。512/1024/1536 三档尺寸、n=1/2
//	                             四次回执全为 5.71（output_tokens 却在 693–1118 间跳），
//	                             说明与尺寸/token/张数都无关；回执带
//	                             output_image_coefficient:1。目录 2026-10-18 曾报牌价
//	                             x5.00（比实扣低 14%），2026-10-22 复测已不再报 credits。
//	global hy-image-3.5          每张固定 0.27 credit。low/medium/high × 512/1024/1536
//	                             六档实测同价，回执恒 output_image_tokens:15000 却**不带**
//	                             coefficient——token 数不代表单价，按 token 反推价格会算错。
//	global gpt-image-2.5-sunburst **已下架**：400 code=14401 route config not found。
//	                             当年按输出图像 token 计费（credit = tokens/7300：1024²
//	                             low 0.55/3997、high 4.84/35333），该口径仅作存档。
package upstream

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 图像通道（chatBase 域，与 growth/report 同 base 同头）。
const (
	imagesPath = "/v2/images/generations"
	editsPath  = "/v2/images/edits"
)

// ImageGenerations 文生图，返回上游 data 对象（OpenAI 形状，可直接透出）。
// 复用 growthJSON：同 chatBase、同 BillingHeaders、同信封解码与 *Error 语义。
func (c *Client) ImageGenerations(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, imagesPath, body)
}

// ImageEdits 图生图（image 为 data URL 数组），同样返回 OpenAI 形状的 data。
func (c *Client) ImageEdits(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, editsPath, body)
}

// ImageChargeAgg 出图实扣按「尺寸 × 画质」分档的聚合（见 RecordImageCharge）。
//
// 为什么需要分档：不同（或不同代）图片模型的计费口径可能完全不同，混成一个均值会骗人：
//
//	cn hunyuan-image-alpha：**每张固定 5.71**，与尺寸/画质/n 全无关，回执带
//	  output_image_coefficient:1（2026-10-22 复测 1024/768/512 三档 + n=2 恒等）；
//	global hy-image-3.5：**每张固定 0.27**，回执恒为 output_image_tokens:15000，
//	  回执不带 coefficient；low/medium/high 与 512/1024/1536 六档实测全部 0.27；
//	global gpt-image-2.5-sunburst（**已下架**，14401 route config not found）：
//	  当年按输出图像 token 计费（credit = tokens/7300，low 0.55/3997、high 4.84/35333）。
//
// 分档仍然保留：它是「这价是哪一档」的唯一凭据，且一旦上游改回按档计价就能直接读出。
//
// 所以面板必须知道某个均值是「哪一档」的平均，否则 0.55 一旦参进 high 样本就再也
// 对不上任何一档真实价格。
type ImageChargeAgg struct {
	Realm   string `json:"realm"`
	Model   string `json:"model"`
	Size    string `json:"size,omitempty"`
	Quality string `json:"quality,omitempty"`
	// Samples 该档累计次数；Credits 累计实扣；Avg 平均每张。
	Samples int     `json:"samples"`
	Credits float64 `json:"credits"`
	Avg     float64 `json:"avg"`
	// Last 最近一次的实扣与输出图像 token（反推单价用）。
	Last       float64 `json:"last"`
	LastTokens int64   `json:"last_output_image_tokens,omitempty"`
	// LastCoefficient 最近一次的 usage.output_image_coefficient（上游给的「倍率」
	// 原值，CN 恒 1、global 不回该字段）。只展示：它是上游自己的口径，本网关
	// 不拿它重算价格——重算出来的数跟回执 credit 对不上就是二次编数。
	LastCoefficient float64 `json:"last_output_image_coefficient,omitempty"`
	LastAt      string  `json:"last_at,omitempty"`
}

// imageChargeKey 分档键：域:模型@尺寸/画质（缺项记 "-"，保证桶不串）。
func imageChargeKey(realm, model, size, quality string) string {
	if size == "" {
		size = "-"
	}
	if quality == "" {
		quality = "-"
	}
	return realm + ":" + model + "@" + size + "/" + quality
}

// RecordImageCharge 记录一次出图实扣（数量取上游回执 usage.credit）。
// tokens 为 usage.output_image_tokens（CN 侧不回该字段，记 0）。
// 只在 credit > 0 时调用（上游省略 usage 时不编数）。
func (c *Client) RecordImageCharge(realm, model, size, quality string, credit float64, tokens int64, coefficient float64) {
	if c == nil || model == "" || credit <= 0 {
		return
	}
	key := imageChargeKey(realm, model, size, quality)
	c.imageChargeMu.Lock()
	defer c.imageChargeMu.Unlock()
	if c.imageCharges == nil {
		c.imageCharges = make(map[string]ImageChargeAgg)
	}
	agg := c.imageCharges[key]
	agg.Realm, agg.Model = realm, model
	if size != "" {
		agg.Size = size
	}
	if quality != "" {
		agg.Quality = quality
	}
	agg.Samples++
	agg.Credits += credit
	agg.Avg = agg.Credits / float64(agg.Samples)
	agg.Last = credit
	if tokens > 0 {
		agg.LastTokens = tokens
	}
	if coefficient > 0 {
		agg.LastCoefficient = coefficient
	}
	agg.LastAt = time.Now().Format(time.RFC3339)
	c.imageCharges[key] = agg
}

// ImageCharges 返回某域某模型的全部分档（样本多的在前）。空结果 = 本次运行还没出过图。
//
// 内存态、重启清零：它是「最近实扣观测」，不是账单；长期口径仍由 usage.Recorder
// （credit_by_model，落盘）负责。刻意不并入 Recorder——那边的桶按 日/小时 分，
// 再加一维尺寸×画质会让整条落盘链路（含旧文件兼容）复杂化，收益不成正比。
func (c *Client) ImageCharges(realm, model string) []ImageChargeAgg {
	if c == nil {
		return nil
	}
	c.imageChargeMu.Lock()
	defer c.imageChargeMu.Unlock()
	out := make([]ImageChargeAgg, 0, 4)
	for _, agg := range c.imageCharges {
		if agg.Realm == realm && agg.Model == model {
			out = append(out, agg)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Samples != out[j].Samples {
			return out[i].Samples > out[j].Samples
		}
		return out[i].Avg > out[j].Avg
	})
	return out
}

// stashGenerationModels 从一次目录解析里留存媒体生成模型（在 nonChatModel 过滤**前**
// 调用，零额外请求），按 GenerationKind 标 image/video。存 Client 上（不是包级全局）：
// 测试各建各的 Client，互不串味。
//
// **按 ID 合并而非覆盖**：global 目录是多 UA 并发探测的，宽目录（WorkBuddy 产品身份，
// 29 个模型）与窄目录（13 个）都会来这里落一笔——覆盖式留存会让结果取决于哪个探测先
// 返回，表现为「这次面板有 gpt-image-2.5-sunburst、下次变成 hunyuan-image-alpha」。
// ponytail: 只增不减，上游下架的模型要等重启才消失；出现真实误报再加版本比对。
func (c *Client) stashGenerationModels(realm string, entries []ModelInfo) {
	media := make([]ModelInfo, 0, len(entries))
	for _, m := range entries {
		if kind := generationKind(m.Tags); kind != "" {
			m.GenerationKind = kind
			media = append(media, m)
		}
	}
	if len(media) == 0 {
		return
	}
	c.imageMu.Lock()
	defer c.imageMu.Unlock()
	if c.imageModels == nil {
		c.imageModels = map[string][]ModelInfo{}
	}
	merged := map[string]ModelInfo{}
	for _, m := range c.imageModels[realm] {
		merged[m.ID] = m
	}
	for _, m := range media {
		merged[m.ID] = m
	}
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, merged[id])
	}
	c.imageModels[realm] = out
}

// sortedInfos 把模型 map 摊成稳定顺序的切片（map 迭代序随机，留存顺序不该抖动）。
func sortedInfos(byID map[string]ModelInfo) []ModelInfo {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

// imageRouteRealm 已实测的「图像模型 → 有路由的域」。
//
// 为什么需要：目录里出现的媒体模型不一定在本域有出图路由——实测 global 目录同样列着
// hunyuan-image-alpha，但国际侧调它必回 400 code=14401「route config not found」。列进
// /v1/models 等于引诱客户端选一个必然失败的模型（客户端枚举列表即踩）。
//
// 2026-10-22 复测（两个个人号，直连上游）：
//   cn:hunyuan-image-alpha 200 → usage{output_image_coefficient:1, credit:5.71}
//   global:hy-image-3.5    200 → usage{output_image_tokens:15000, credit:0.27}
//   global:gpt-image-2.5-sunburst → **400 code=14401 route config not found**
//     （已下架；两个域的目录里都不再有它，global 目录把可用出图模型只放在
//     modelPromotions 里，见 fetchV3ConfigModelMap 的补入逻辑）
// 所以 sunburst 这条映射已作废：留着只会把一个必失败的模型列给客户端。
//
// ponytail: 只写实测过的组合，未知模型照旧列出（不猜）；图像模型变多时改成探测一次路由
// 并缓存——现在只有这几个模型，加探测是白花钱。
var imageRouteRealm = map[string]string{
	"hunyuan-image-alpha":      "cn",
	"hunyuan-image-alpha-edit": "cn",
	"hy-image-3.5":             "global",
}

// imageDeadModels 上游已下架、两个域都没有路由的出图模型：列出来只会让客户端
// 选到一个必然 400 的模型。实测 2026-10-22：global:gpt-image-2.5-sunburst
// → 400 code=14401「route config not found」，且两域目录里都已不再出现它。
// 只写实测下架的；模型回来时删一行即可（未知模型照旧列出，见 GenerationModels）。
var imageDeadModels = map[string]bool{
	"gpt-image-2.5-sunburst": true,
}

// GenerationModels 最近一次目录解析留存的出图/出视频模型（空 = 尚未探测过该域）。
// 目录把这些模型从**对话**列表剔掉（客户端拿它发对话会 11102），留存只供 /v1/models
// 与面板以 image_generation 标记列出（视频不列：网关没有视频端点）。
func (c *Client) GenerationModels(realm string) []ModelInfo {
	c.imageMu.Lock()
	defer c.imageMu.Unlock()
	src := c.imageModels[realm]
	out := make([]ModelInfo, 0, len(src))
	for _, m := range src {
		// 已下架的模型不列（见 imageDeadModels）。
		if imageDeadModels[m.ID] {
			continue
		}
		// 已知在别的域才有路由的模型不列（见 imageRouteRealm）。
		if r, known := imageRouteRealm[m.ID]; known && r != realm {
			continue
		}
		out = append(out, m)
	}
	return out
}

// StashGenerationModelsForTest 是 stashGenerationModels 的导出钩子：server 包要验证
// 「出图模型单列到 /v1/models 且受白/黑名单管辖」，而留存只由目录解析路径写入
// （跨包无法直接构造）。见 AGENT.md「跨包的私有常量用导出的 ...ForTest 钩子」。
func (c *Client) StashGenerationModelsForTest(realm string, entries []ModelInfo) {
	c.stashGenerationModels(realm, entries)
}
