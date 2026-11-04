// bodylimit.go 入站请求体上限的唯一实现。
//
// 为什么单独成文件：`server.max_body_mb` 的 `http.MaxBytesReader` 原先只在
// `chatCompletions` 里套一次，而 `/v1/messages`、`/messages/count_tokens`、
// `/v1/responses`、`/v1/images/*` 这四条路径在**调用 chatCompletions 之前**就已经
// 把原始 body `io.ReadAll` 进内存、再 `Unmarshal` 成 `map[string]any` 了 ——
// 一个 300MB 的 POST 先产生 300MB `[]byte`，再膨胀成数倍大小的 interface 树，
// RSS 直接上 GB，进程被 OOM 杀掉，整池账号与所有在途 SSE 一起断。
// `ReadTimeout` 只限时间不限体积，挡不住这个。
//
// 所以上限必须在**每个**读体入口之前套上，且只由这里算，避免五处各写一份再漂移。
package server

import (
	"errors"
	"fmt"
	"net/http"
)

// bodyLimitFallbackMB `server.max_body_mb` 配成 <=0（"不限制"）时实际生效的硬上限。
//
// 为什么不再真的"不限制"：ReadTimeout 只管时间，body 体积没有任何其它兜底，
// 一个畸形请求就能打死进程并连带打断所有在途客户端。32MB 远超任何真实请求
// （100 万 token 的 JSON 约 4MB），只挡内存炸弹。
const bodyLimitFallbackMB = 32

// limitBody 给入站请求体套上上限，返回实际生效的字节数（供 413 文案用）。
func (h *Handler) limitBody(w http.ResponseWriter, r *http.Request) int64 {
	limit := int64(bodyLimitFallbackMB) << 20
	if h != nil && h.cfg.MaxBodyMB > 0 {
		limit = int64(h.cfg.MaxBodyMB) << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return limit
}

// bodyTooLarge 报告读体错误是否是「超过上限」——调用方据此回 413 而不是 400：
// 413 让客户端知道该缩小请求，400 只会让它重试。
func bodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// bodyLimitText 413 文案（五种入口共用同一句话，避免同一个事实两种说法）。
func bodyLimitText(limit int64) string {
	return fmt.Sprintf("request body exceeds server.max_body_mb (%d MB)", limit>>20)
}
