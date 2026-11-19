// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// allowEmptyKey 「空 key = 未启用鉴权」这个缺省语义的显式开关（审查 W2-AU-01）。
//
// 背景：key 为空时 VerifyBearer/VerifyDual 恒返回 true —— 这是 fail-open。默认保持
// 这个语义是刻意的：server 的 /v1/* 与 panel 都靠它表达「本部署没配 api_key」，
// 把默认改成 fail-closed 会让所有「回环 + 无密钥」的本地部署（含测试）直接 401。
//
// 但 fail-open 不该是**不可关闭**的：配置漏填密钥（把 api_key 写成空串、env 覆盖
// 成空）时，网关会安静地对全世界开放。这里给部署方一个显式闸口：
//
//	httpauth.SetAllowEmptyKey(false)  // 之后空 key 只允许回环请求（失败关闭）
//
// W5-04：关闭语义是「拒绝所有**非回环**请求」而不是「连本机也拒」——Android 内嵌
// 形态与单机自托管的所有流量都来自 127.0.0.1，全拒会让「漏配 api_key」直接变成
// 服务不可用，运维只能把开关改回去，反而更危险。回环判定只看 RemoteAddr
// （不看 X-Forwarded-For，客户端可伪造）；反代部署下 RemoteAddr 是反代自己，
// 那正是「本机反代 = 公网入口」的场景，必须在反代层配密钥，不能指望这里放行。
//
// 语义只影响「key 为空」这一条分支，非空 key 的比较路径完全不变（仍是
// SHA-256 + ConstantTimeCompare），因此不存在「关了开关反而更弱」的组合。
//
// 注：面板侧对敏感接口另有独立加固（internal/panel/panel.go 在 api_key 为空时对
// 导出/配置/导入/登录/账号增删直接 403）；本开关是给 server 侧与未来调用方的
// 统一闸口，不替代面板那一层。
var allowEmptyKey atomic.Bool

func init() { allowEmptyKey.Store(true) }

// SetAllowEmptyKey 设置「空 key 是否放行」。false = 失败关闭（空 key 一律拒绝）。
// 进程级设置，应在装配阶段（启动时）调用，不要按请求切换。
func SetAllowEmptyKey(allow bool) { allowEmptyKey.Store(allow) }

// AllowEmptyKey 报告当前语义（默认 true）。
func AllowEmptyKey() bool { return allowEmptyKey.Load() }

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，按 AllowEmptyKey() 决定是否放行（默认放行，见上）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return AllowEmptyKey() || isLoopbackRequest(r)
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
		return AllowEmptyKey() || isLoopbackRequest(r)
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

// isLoopbackRequest 判断请求是否来自本机（W5-04）。
//
// 只看 RemoteAddr：X-Forwarded-For / X-Real-IP 是客户端可写头，拿它当判据等于
// 「加一行头就绕过鉴权」。RemoteAddr 解析不出来（空串/异常形态）一律按非回环处理，
// 失效方向必须是拒绝。
func isLoopbackRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
