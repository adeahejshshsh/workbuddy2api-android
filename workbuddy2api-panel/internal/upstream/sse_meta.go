// sse_meta.go SSE 帧内元数据行的上限与 data 行的多行写出（审查 W1-03 / W1-04）。
package upstream

import (
	"io"
	"strings"
)

// sseMetaMaxLines / sseMetaMaxBytes 一个事件内 `event:` / `id:` / `retry:` 元数据行的
// 双上限（审查 W1-03）。
//
// 缺陷：这些行只追加、只在 writeMeta（写 data 帧 / 流尾）时清空。上游只要持续发
// `event:` / `id:` 而**不发 data**，缓冲就无上限增长 —— 单行上限是 MaxSSELineBytes
// (8MB)，一万行就是 80GB，而 Go 的 OOM 是进程级，全池一起死。
//
// 取值：正常事件最多 2–3 行（event + id，个别带 retry），64 行 / 8KB 已留出一个
// 数量级余量；超限时「先刷帧再追加」，字节一个不丢，只是把它们拆成多个事件帧。
const (
	sseMetaMaxLines = 64
	sseMetaMaxBytes = 8 << 10
)

// writeDataEvent 把一帧 payload 写成一个（可能多行的）data 事件（审查 W1-04）。
//
// SSE 规范：一个事件的多行 data: 以 \n 拼接成消息体，客户端逐行剥掉 `data: ` 前缀
// 再拼回。此前多行 data 拼出的**非 JSON** 内容被整块塞回单行 `data: a\nb\n\n` ——
// 客户端按行切分，第二行起没有字段名，按规范必须忽略，内容静默只剩第一行。
//
// 单行 payload（绝大多数帧）与旧输出逐字节一致；`[DONE]` 是单行哨兵，由调用方直接
// 写出（见 StreamHint 流尾），不进这里。
func writeDataEvent(w io.Writer, payload string) error {
	if !strings.Contains(payload, "\n") {
		_, err := io.WriteString(w, "data: "+payload+"\n\n")
		return err
	}
	for _, ln := range strings.Split(payload, "\n") {
		if _, err := io.WriteString(w, "data: "+ln+"\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
