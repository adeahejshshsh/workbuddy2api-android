// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// VerifyDual 同时接受 `Authorization: Bearer <key>` 与 Anthropic 的
// `x-api-key: <key>` 两种携带方式。
//
// 为什么需要：GET /v1/models 是双协议共用端点，官方 Anthropic SDK 列模型走 x-api-key
// —— 同一份密钥能调通对话却列不出模型，用户只会以为网关坏了。
//
// 向后兼容：Authorization 存在时**只**按 Bearer 判（既有的错误密钥也照旧被拒），
// 仅当它为空时才提升 x-api-key；两条路的比较都走同一个常量时间摘要，没有第二条
// 比较路径，也就没有引入新的侧信道。
func VerifyDual(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), bearerPrefix) {
		return VerifyBearer(r, key)
	}
	alt := strings.TrimSpace(r.Header.Get("x-api-key"))
	if alt == "" {
		// 两种都没带：仍走一次摘要比较，保持耗时形状一致。
		return VerifyBearer(r, key)
	}
	return subtle.ConstantTimeCompare(digest(alt), digest(key)) == 1
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
