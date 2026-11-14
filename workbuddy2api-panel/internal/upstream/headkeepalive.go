// headkeepalive.go 首帧心跳：本账号上游已回 200、首帧还没到的空窗里，向客户端补 SSE 注释帧。
//
// 为什么需要：StreamHint 是「首帧否决」设计——首个有效帧到达前一个字节都不写（首帧就是
// error 帧时可以无痕换号），代价是客户端从建连起零字节干等整个首帧延迟。大上下文的预填充
// 把它拉到秒级（线上实测 160–320k 档 TTFB 中位 2.6s / P90 4.8s，「零内容 + client_gone」的
// 中断全部落在 3.46~4.09s），等首字节有超时的客户端会自己放弃。注释帧（": " 开头的行）是
// SSE 规范的 no-op，唯一作用是让连接上有数据流动。
//
// 三条不变量：
//   - 只在 StreamHint 里起：此时本账号的上游响应头已经是 200，HTTP 层的 429/503/400 早已按
//     原状态码处理（换号、Retry-After、X-Wb-Account 都不受影响）。在选号之前就写注释，
//     一旦写过，之后所有失败都只能变成「200 + error 帧」。
//   - 所有写都在 StreamHint 自己的协程里：辅助协程只替它读上游的下一行（一问一答），它在
//     等这一行时按节拍写注释。没有第二个协程碰 ResponseWriter，更没有协程去改响应头——
//     响应头在 StreamHint 开头已设好，第一条注释提交的就是它们（http.Header 是普通 map，
//     两个协程一起 Set 会让运行时直接 fatal）。
//   - 首个 data 帧一到（首帧否决窗口关闭）就不再心跳，之后直接读，不经辅助协程。每次
//     返回都发生在「这一问已经拿到答复」之后，所以退出时没有悬着的读。
//
// 仍然存在的代价：注释帧提交了 200。这一次尝试若随后被首帧否决（第一帧是 error 帧），
// handler 照常换号，但客户端已经收到 200：换号成功时客户端无感（只是多了几行注释，
// X-Wb-Account 是首个账号的）；全部失败时只能以协议内 error 帧收尾（server 侧
// sseCommitWriter 的已提交分支）。
package upstream

import (
	"bufio"
	"time"
)

// WithHeadKeepalive 打开首帧心跳：首个 data 帧到达之前，每 every 向客户端写一条
// ": keepalive" 注释帧。every<=0 = 关闭（与不挂一致）。
func WithHeadKeepalive(every time.Duration) StreamOption {
	return func(o *streamOptions) { o.headKeepalive = every }
}

// headLine 辅助协程的一次答复。
type headLine struct {
	line string
	err  error
}

// headHeartbeat 首帧窗口内替 StreamHint 读行的辅助协程（见文件头的不变量）。
type headHeartbeat struct {
	every time.Duration
	next  time.Time // 下一条注释的时刻（只由调用方协程读写）
	req   chan struct{}
	resp  chan headLine
	quit  chan struct{}
}

func newHeadHeartbeat(br *bufio.Reader, every time.Duration) *headHeartbeat {
	h := &headHeartbeat{every: every, next: time.Now().Add(every),
		req: make(chan struct{}), resp: make(chan headLine, 1), quit: make(chan struct{})}
	go func() {
		for {
			select {
			case <-h.quit:
				return
			case <-h.req:
				line, err := br.ReadString('\n')
				h.resp <- headLine{line, err} // 缓冲 1：调用方总会来取，不会卡住
			}
		}
	}()
	return h
}

// readLine 请辅助协程读一行，等待期间到点就调一次 tick（在调用方协程里）。节拍跨行连续计时：
// 上游自己的空行/注释行不能把心跳一直往后推。返回时这一问的答复已经拿到，辅助协程回到等待
// 状态，调用方此后可以直接读 br。
func (h *headHeartbeat) readLine(tick func()) (string, error) {
	h.req <- struct{}{}
	t := time.NewTimer(time.Until(h.next))
	defer t.Stop()
	for {
		select {
		case r := <-h.resp:
			return r.line, r.err
		case <-t.C:
			tick()
			h.next = time.Now().Add(h.every)
			t.Reset(h.every)
		}
	}
}

// stop 让辅助协程退出（只调一次）。
func (h *headHeartbeat) stop() { close(h.quit) }
