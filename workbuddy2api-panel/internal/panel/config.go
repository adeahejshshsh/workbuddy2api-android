// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	// 含密钥的 JSON 不能被中间代理/浏览器缓存（审查 P0-1 次生项）。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.ConfigPath,
		"config": redactConfig(cfg),
	})
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	// 多读 1 字节判断超长（bug P2-3）：外层 ServeHTTP 允许 32MB，这里 1MB 之外
	// 被静默截断 → SaveConfig 报 "unexpected end of JSON input"，用户看到的是语法
	// 错误，根本不知道是体积超了。
	raw, err := io.ReadAll(io.LimitReader(r.Body, panelMaxJSONBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(raw) > panelMaxJSONBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			"config body too large (limit 1MB)")
		return
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}

// redactedValue 掩码占位符。**保存时把值等于它的字段视为「未修改」**（见
// cmd/server 的 restoreRedactedSecrets），所以它必须是任何真实取值都不会碰巧相等的串。
// RedactedValue 导出给 cmd/server 对齐用（保存时把等值字段视为「未修改」）。
const RedactedValue = redactedValue

const redactedValue = "\u2022\u2022\u2022redacted\u2022\u2022\u2022"

// sensitiveConfigKeys 面板 GET 必须打码的字段（按 key 名匹配，大小写不敏感，
// 递归进 map/slice）。判断口径是「泄漏它等于泄漏凭据」，不是「看起来敏感」——
// 多打码只会让运维多填一次，漏打码就是一次凭据外泄。
var sensitiveConfigKeys = map[string]bool{
	"api_key":             true,
	"admin_api_key":       true,
	"token":               true, // upstash.token / alerting 里的各种 token
	"secret":              true, // alerting.secret（webhook HMAC）
	"device_token":        true,
	"three_d_temp_tokens": true, // map[uid]ck_t_，整表打码
	"webhook_url":         true, // URL 里常带凭据（?token=…）
	"password":            true,
	"refresh_token":       true,
	"access_token":        true,
}

// redactConfig 深拷贝一份并把敏感字段替换成掩码，保持结构不变（前端仍能渲染表单）。
func redactConfig(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			if sensitiveConfigKeys[strings.ToLower(k)] {
				out[k] = maskAny(vv)
				continue
			}
			out[k] = redactConfig(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = redactConfig(vv)
		}
		return out
	}
	// 结构体走 JSON 往返（LoadConfig 返回的具体类型由 main 决定）。
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var generic any
	if json.Unmarshal(raw, &generic) != nil {
		return v
	}
	if m, ok := generic.(map[string]any); ok {
		return redactConfig(m)
	}
	return generic
}

// maskAny 把任意敏感值替换成掩码；空值保持空（「没配」本身不是秘密，且面板要能区分）。
func maskAny(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return ""
		}
		return redactedValue
	case map[string]any:
		if len(t) == 0 {
			return map[string]any{}
		}
		return redactedValue
	}
	// 数字/布尔等：非空即视为已配置。
	if s, ok := jsonNumberish(v); ok {
		return s
	}
	return redactedValue
}

// jsonNumberish 数字/布尔原样回显（它们不是凭据，且面板表单要显示真实值）。
func jsonNumberish(v any) (any, bool) {
	switch v.(type) {
	case float64, bool, int, int64:
		return v, true
	}
	return nil, false
}
