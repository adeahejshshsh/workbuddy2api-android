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
//
// 为什么不用 http.MaxBytesReader（上游 b494426 的取舍，2026-11-07 合并）：
// MaxBytesReader 超限后会把内部错误钉死并让 net/http 关连接，客户端往往只看到
// connection reset，看不到那份 413 —— 它还在上传，服务端已经把连接掐了。这里改成
// 「自己数字节 + 超限后仍能排空」的包装：413 之前先把剩余上传读掉（限 2s），
// 客户端才真的能读到「你的请求太大」。
package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// bodyLimitFallbackMB `server.max_body_mb` 配成 <=0（"不限制"）时实际生效的硬上限。
//
// 为什么不再真的"不限制"：ReadTimeout 只管时间，body 体积没有任何其它兜底，
// 一个畸形请求就能打死进程并连带打断所有在途客户端。32MB 远超任何真实请求
// （100 万 token 的 JSON 约 4MB），只挡内存炸弹。
const bodyLimitFallbackMB = 32

// drainDeadline 超限后排空剩余上传的时限：不让一个慢速客户端把连接和 goroutine 拖住。
const drainDeadline = 2 * time.Second

// errBodyTooLarge 超限哨兵。与 http.MaxBytesError 一样能过 errors.Is/As 判断，
// 区别只在于超限后底层 body 仍可读——排空要用到这一点。
var errBodyTooLarge = errors.New("http: request body too large")

// bodyLimiter 数着字节读的 body 包装：多读的那一个字节就是「超限」的证据
// （不看 Content-Length——头可以撒谎，读进去的字节不会）。
type bodyLimiter struct {
	rc    io.ReadCloser
	limit int64
	read  int64
	over  bool
}

func (b *bodyLimiter) Read(p []byte) (int, error) {
	if b.over {
		return 0, errBodyTooLarge
	}
	// 只读到「上限 + 1」为止：多一个字节即可判定超限，不必把整段读进来。
	if remaining := b.limit - b.read + 1; int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.rc.Read(p)
	b.read += int64(n)
	if b.read > b.limit {
		b.over = true
		return n, errBodyTooLarge
	}
	return n, err
}

func (b *bodyLimiter) Close() error { return b.rc.Close() }

// drain 排空剩余上传（最多 limit 字节或 drainDeadline 到点即停）。
//
// 超限时**必须**做这一步：直接 return 会让 net/http 关掉连接，客户端只看到
// connection reset，看不到那份 413（hub #799 的同款坑）。
func (b *bodyLimiter) drain(w http.ResponseWriter, limit int64) {
	if w != nil {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(drainDeadline))
	}
	_, _ = io.CopyN(io.Discard, b.rc, limit)
}

// limitBody 给入站请求体套上上限，返回实际生效的字节数（供 413 文案用）。
func (h *Handler) limitBody(w http.ResponseWriter, r *http.Request) int64 {
	limit := int64(bodyLimitFallbackMB) << 20
	if h != nil && h.cfg.MaxBodyMB > 0 {
		limit = int64(h.cfg.MaxBodyMB) << 20
	}
	if _, ok := r.Body.(*bodyLimiter); !ok {
		r.Body = &bodyLimiter{rc: r.Body, limit: limit}
	}
	return limit
}

// drainBody 413 之前排空剩余上传。body 不是本文件的包装时（防御路径）退化为
// 按上限读掉有限字节，语义相同。
func drainBody(w http.ResponseWriter, r *http.Request, limit int64) {
	if bl, ok := r.Body.(*bodyLimiter); ok {
		bl.drain(w, limit)
		return
	}
	if w != nil {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(drainDeadline))
	}
	_, _ = io.CopyN(io.Discard, r.Body, limit)
}

// bodyTooLarge 报告读体错误是否是「超过上限」——调用方据此回 413 而不是 400：
// 413 让客户端知道该缩小请求，400 只会让它重试。
//
// 两种哨兵都认：本文件的 errBodyTooLarge，以及历史路径上可能残留的 *http.MaxBytesError
// （第三方中间件替我们套过 MaxBytesReader 时）。
func bodyTooLarge(err error) bool {
	if errors.Is(err, errBodyTooLarge) {
		return true
	}
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// bodyLimitText 413 文案（五种入口共用同一句话，避免同一个事实两种说法）。
func bodyLimitText(limit int64) string {
	return fmt.Sprintf("request body exceeds server.max_body_mb (%d MB)", limit>>20)
}
