// taskowner.go 任务台账的**调用方归属**（审查 A6）。
//
// 视频 / 3D 的上游任务只对提交它的账号可见，但网关的台账此前只记「哪个账号提交的」，
// 不记「哪个下游调用方提交的」——于是任一持 key 者拿到（或猜到）别人的 task_id / job_id
// 就能查到成品直链（COS 直链带 q-signature，12–24 小时有效），等于越权取产物。
//
// 这里给出调用方指纹：对 **Authorization 头** 做 sha256 取前 16 位十六进制。它不落盘、
// 不进日志，只在进程内台账里做等值比较；网关不配 key（全开放）时返回空串 = 不做归属校验
// （那种部署本来就没有租户边界可言）。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// callerPrint 当前请求的调用方指纹（空串 = 不做归属校验）。
func (h *Handler) callerPrint(r *http.Request) string {
	if r == nil {
		return ""
	}
	// 没配网关 key = 对外开放且无租户边界：不做归属校验，免得把既有行为改成 404。
	if key, _ := h.panelAdminKeyForScope(); key == "" {
		return ""
	}
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(authz))
	return hex.EncodeToString(sum[:8])
}

// panelAdminKeyForScope 只看「网关是否要求鉴权」：与鉴权语义同源（Live 优先）。
func (h *Handler) panelAdminKeyForScope() (string, bool) {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load().APIKey, true
	}
	return h.cfg.APIKey, true
}
