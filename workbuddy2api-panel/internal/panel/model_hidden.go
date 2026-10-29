// model_hidden.go 模型能力页的行内「下游可见」开关：把单个模型增删进
// config models.hidden，并经 main 注入的 SaveConfig 走校验+落盘+热生效
// （livecfg 快照重建，下一次 /v1/models 与点名调用立即按新状态执行）。
//
// 只提交 models.hidden 一个键：main.saveConfig 对嵌套对象做深合并，models.allowed /
// models.aliases 等兄弟键保持用户原样。
package panel

import (
	"encoding/json"
	"net/http"
	"strings"
)

// setModelHidden 切换单个模型的「下游可见」状态。
//
// 请求：POST /panel/api/model_hidden  {"id":"cn:glm-5.2","hidden":true}
// id 必须带域前缀（面板展示 id 即调用值）；hidden=true 进黑名单，false 移出。
func (p *Panel) setModelHidden(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil || p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	var req struct {
		ID     string `json:"id"`
		Hidden bool   `json:"hidden"`
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

	// 幂等增删：已存在时先剔除再按请求追加，避免重复条目（hiddenModel 按集合判定，
	// 但配置里出现重复会让人看不懂面板的黑名单输入框）。
	out := make([]string, 0, len(hiddenList(root))+1)
	for _, it := range hiddenList(root) {
		if strings.EqualFold(it, id) {
			continue
		}
		out = append(out, it)
	}
	if req.Hidden {
		out = append(out, id)
	}
	body, err := json.Marshal(map[string]any{"models": map[string]any{"hidden": out}})
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
		"hidden":  req.Hidden,
		"entries": len(out),
	})
}

// hiddenList 取 config.models.hidden，保留用户原有顺序与条目写法
// （裸名 = 两域同时生效，cn:/global: 只该域）；非字符串条目忽略。
func hiddenList(root map[string]any) []string {
	models, _ := root["models"].(map[string]any)
	if models == nil {
		return nil
	}
	items, _ := models["hidden"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
