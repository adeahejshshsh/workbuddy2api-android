// busy.go 对外暴露官方客户端的「模型忙」预检端点：POST /v2/chat/busy。
//
// 为什么需要它（2026-10-21 真机抓包）：
//   - 国内版 5.8.3 / 国际版 5.8.4 客户端在每次对话前都会调这个端点，body 形如
//     {"models":["deepseek-v4.1-flash", ...]}，官方上游回
//     {"code":0,"msg":"OK","data":[{"model":"...","busy_level":"idle|busy|unknown"}]}，
//     客户端据此给模型选择器打「繁忙」标记。
//   - 国区上游该路径返回 200（真机实测）；国际版同一路径返回 404 Route Not Found
//     —— 客户端会自己兜住，但**网关**完全没实现这条路径时，用网关当后端的客户端
//     （自建面板/脚本）拿不到任何忙闲信息，只能靠 503 撞出来。
//
// 实现选择：**本地按池状态算**，不打上游。理由：
//   - 上游国区虽可用，但国际版 404，透传会让两个 realm 行为不一致；
//   - 网关自己就有权威的模型级冷却台账（modelCooldowns），比上游的
//     "unknown" 更准——上游对聚合别名（fast-model/balanced-model/deep-model）
//     一律回 unknown，等于没有信息；
//   - 不引入额外上游调用，不消耗额度、不放大风控面。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// busyLevel 取值与官方一致：idle / busy / unknown。
const (
	busyLevelIdle    = "idle"
	busyLevelBusy    = "busy"
	busyLevelUnknown = "unknown"
)

// busyMaxModels /v2/chat/busy 单请求最多接受的模型条数。
//
// W2-05：输出与 len(req.Models) 线性放大（每条一个 {model,busy_level} 对象 +
// 一次 servableModelSet 查表），而 body 上限是 1MiB。一个 1MiB 的 {"models":[...]}
// 能塞进约 6 万条，响应 60-75MB、堆上还要再留一份 map 切片——没有上限时这是一条
// 零成本的放大器。64 条已远超官方客户端（一次只问当前选择器里的十几个模型）。
const busyMaxModels = 64

// chatBusy 处理 POST /v2/chat/busy。
//
// 语义：每个请求模型映射成 busy_level
//   - busy：该模型在本域所有可用账号上都处于模型级冷却（等同 chat 会走到
//     model_busy / model_unavailable 的那一档）；
//   - idle：还有账号能服务该模型；
//   - unknown：模型不在本网关的服务目录里（拼错/未适配/被隐藏），或请求为空。
//
// 未知模型回 unknown 而不是 idle：这是与官方口径一致的选择（官方对无法判定的
// 模型也回 unknown），且不会让客户端误以为可以选。
func (h *Handler) chatBusy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	// W2-05：超限 400，不能先构造再判——构造本身就是要防的放大点。
	if len(req.Models) > busyMaxModels {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("too many models: %d (max %d)", len(req.Models), busyMaxModels))
		return
	}
	out := make([]map[string]any, 0, len(req.Models))
	known := h.servableModelSet()
	for _, m := range req.Models {
		realm, bare := resolveModel(m)
		lvl := busyLevelUnknown
		if _, ok := known[bare]; ok {
			if h.cfg.Pool != nil && h.cfg.Pool.ModelBlockedForRealm(bare, realm).Blocked {
				lvl = busyLevelBusy
			} else {
				lvl = busyLevelIdle
			}
		}
		out = append(out, map[string]any{"model": m, "busy_level": lvl})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":      0,
		"msg":       "OK",
		"requestId": session.NewMessageID(),
		"data":      out,
	})
}

// servableModelSet 返回网关当前对外提供的模型 id 集合（裸名，不含 realm 前缀）。
//
// 取 /v1/models 同一份动态目录缓存（modelList 内部已按 hidden_models 过滤，
// 这里不再重复过滤）；缓存为空（还没探测过/探测失败）时回空集，调用方据此把
// 模型判为 unknown —— 不编造「这个模型我们有」。
func (h *Handler) servableModelSet() map[string]struct{} {
	infos := h.modelList()
	set := make(map[string]struct{}, len(infos))
	for _, it := range infos {
		id, _ := it["id"].(string)
		if id == "" {
			continue
		}
		_, bare := resolveModel(id)
		set[bare] = struct{}{}
	}
	return set
}
