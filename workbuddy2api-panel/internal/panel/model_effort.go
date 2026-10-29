// model_effort.go 模型能力页的行内「网关档位」下拉：把单个模型的网关目标思考档位
// 写进 config thinking.model_defaults（键 "cn:模型名" / "global:模型名"），经 main
// 注入的 SaveConfig 走 normalizeThinking 校验 + 落盘 + 热生效（livecfg 快照与
// upstream.Client 的 EffortPolicy 同批更新，下一次出站请求即按新档位注入）。
//
// 只提交 thinking.model_defaults 一个键：main.saveConfig 对嵌套对象做深合并，
// thinking 的兄弟键（enabled / default_effort / client_priority / override_scope）
// 保持用户原样。档位只增改不删；值可以是具体档位或 off（该模型固定关闭思考）。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// setModelEffort 设置单个模型的网关目标思考档位。
//
// 请求：POST /panel/api/model_effort  {"id":"cn:glm-5.2","effort":"high"}
// id 必须带域前缀（面板展示 id 即调用值）；effort 取 max/minimal/low/medium/high/
// xhigh/off。off 表示该模型网关侧固定关闭思考（面板只给 canDisableThinking 的
// 模型列出该选项；scope=level 下客户端显式档位仍优先，见 ensureEffortPolicy）。
func (p *Panel) setModelEffort(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil || p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	var req struct {
		ID     string `json:"id"`
		Effort string `json:"effort"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	id := strings.TrimSpace(req.ID)
	realm, bare, ok := strings.Cut(id, ":")
	if !ok || bare == "" || (realm != "cn" && realm != "global") {
		writeErr(w, http.StatusBadRequest, "id 需为 cn:模型名 或 global:模型名")
		return
	}
	effort := strings.ToLower(strings.TrimSpace(req.Effort))
	if effort == "" {
		writeErr(w, http.StatusBadRequest, "effort 不能为空（默认取模型支持的最高档；off = 关闭该模型思考）")
		return
	}
	if !upstream.IsEffortLevel(effort) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("effort %q 不是合法档位（max/xhigh/high/medium/low/minimal/off）", req.Effort))
		return
	}

	cfgAny, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	raw, err := json.Marshal(cfgAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal config: "+err.Error())
		return
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		writeErr(w, http.StatusInternalServerError, "decode config: "+err.Error())
		return
	}

	// 保留既有条目（裸名两域共用、别的域的精确键、已删除模型的残留键都原样带上），
	// 只覆盖当前模型的键；键统一小写，与 EffortPolicy.DefaultEffortFor 的查找口径一致。
	entries := effortDefaults(root)
	entries[realm+":"+strings.ToLower(bare)] = effort
	body, err := json.Marshal(map[string]any{"thinking": map[string]any{"model_defaults": entries}})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal config: "+err.Error())
		return
	}
	if _, err := p.cfg.SaveConfig(body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"id":      id,
		"effort":  effort,
		"entries": len(entries),
	})
}

// effortDefaults 取 config.thinking.model_defaults 现值（保留用户原有键写法与顺序
// 无关）；非字符串条目忽略，避免把脏数据再喂回校验。
func effortDefaults(root map[string]any) map[string]string {
	out := map[string]string{}
	thinking, _ := root["thinking"].(map[string]any)
	if thinking == nil {
		return out
	}
	m, _ := thinking["model_defaults"].(map[string]any)
	for k, v := range m {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out[k] = s
		}
	}
	return out
}
