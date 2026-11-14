// windowprobe.go 额度窗口探针（借鉴 ardeyouxipianyi/workbuddy2api-hub 的 wb_window_probe.py）。
//
// 登记侧在 pool/window.go：429 带「将在 … 重置」时记一条窗口。本文件是执行侧——
// 到点后为那个 (账号, 模型) 发一个最小请求，把新一轮配额窗口的计时起点钉在重置
// 那一刻。
//
// 为什么必须主动发：上游按**首次请求**起算窗口。重置时刻到了却没人发请求，窗口就
// 不开始，额度一直空转；等下一条真实请求进来，起点落到那一刻——通常晚了几分钟到
// 几十分钟，那段时间的额度等于白扔。我们原来的处理只到「解除冷却」（允许请求进来），
// 是被动的。
//
// 结果处置：
//   - 首个有效帧是正常内容 → 窗口已开：清掉该 (账号,模型) 的 6004 冷却，删窗口记录；
//   - 仍限流且给了更晚的重置时刻 → 上游确实开了新一轮，把 ResetAt 换成新的，计数归零；
//   - 仍限流、时刻没变（服务端时钟/处理延迟）→ 过一会儿重试，连续 windowProbeMaxAttempts
//     次后放弃（删记录，下一次真实 429 会重新登记）；
//   - 其它错误（网络、鉴权、5xx、200 流里的非限流错误帧）→ 不重试，删记录：真实流量照常会
//     开窗，探针是真实计费的对话请求，不值得为它反复打上游。
//
// 探针与选号同一套闸门（pool.WindowProbeBlocked）：停用、暂停、触及保留积分或积分保底、
// 免费号碰订阅专属模型时不发，直接删记录。
//
// 刻意**不**做的事：主动扫描还没被限流的「账号 × 模型」配额线（hub 的
// wb_quota_scan）。那是另一种功能：它要用探针去**发现**哪条线满了，请求量随
// 账号×模型数增长，与「已经知道满了、只等起窗」不是一件事。混在一起会让本机制的
// 请求量从「每个已登记窗口一次」变成「每 6 小时 × 账号数 × 模型数」。
package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// windowProbePrompt / windowProbeMaxTokens 探针请求要尽量小：它唯一的目的是让
	// 上游把这一轮窗口的计时起点定在此刻，产出什么都不重要。
	windowProbePrompt    = "Hi"
	windowProbeMaxTokens = 16
	// windowProbeTimeout 单次探针的硬上限。取 60s：上游劣化期 TTFB 可能到几十秒
	// （见 internal/server/upstreamhealth.go 的实测），但探针不值得为它等更久。
	windowProbeTimeout = 60 * time.Second
	// windowProbeRetryDelay 到点后上游常还没真正放开（服务端时钟/处理延迟几秒到
	// 几分钟）。15s 首次重试：太密是白打上游，太疏会错过窗口起点。
	windowProbeRetryDelay = 15 * time.Second
	// windowProbeMaxAttempts 连续这么多次探针仍未开窗就放弃。20 × 15s ≈ 5 分钟，
	// 覆盖「上游声明时刻比实际早几分钟」的常见偏差。
	windowProbeMaxAttempts = 20
	// windowProbeFreshMin 429 给的重置点比「现在 + 这个值」还远，才算上游真的开了
	// 一轮新窗口；否则只是探针发早了，仍按同一轮重试。
	windowProbeFreshMin = 30 * time.Second
	// windowProbeBatchLimit 一轮最多处理几个窗口：一个卡住的上游不该把巡检线程占满。
	windowProbeBatchLimit = 8
	// windowProbeGap 多个窗口同时到点时错开，别在同一秒向上游打一串请求。
	windowProbeGap = 400 * time.Millisecond
	// windowProbeIdle 没有待处理窗口时的巡检间隔。有窗口时按最近的 ResetAt 精确
	// 醒来（见 Run 循环），这个值只在完全空闲时用于兜底——比如面板手工登记、
	// 或另一个进程写了 state.json。
	windowProbeIdle = 30 * time.Second
)

// windowProbeBody 构造探针请求体。
//
// stream:true 是必须的（上游只认流式，非流式会 400/挂住，同 /v1/chat/completions
// 的 stream:false 路径也是按 SSE 聚合出来的）。
func windowProbeBody(model string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": windowProbePrompt},
		},
		"stream":     true,
		"max_tokens": windowProbeMaxTokens,
	})
}

// StartModelWindowProbe 启动额度窗口探针循环（独立 goroutine，ctx 取消即停）。
//
// 与 StartBalanceRefresh 同构：独立于 Run 的小时制排程——窗口到点是秒级事件，
// 不值得为它扩展 nextFire 的粒度。
func (s *Scheduler) StartModelWindowProbe(ctx context.Context) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	go func() {
		for {
			due := s.cfg.Pool.DueModelWindows(time.Now())
			if len(due) > 0 {
				s.runWindowProbes(ctx, due)
				if ctx.Err() != nil {
					return
				}
				// 一轮处理完立刻回到循环顶重判（可能还有待处理窗口）。
				continue
			}
			wait := windowProbeIdle
			if next, ok := s.cfg.Pool.NextModelWindowDue(); ok {
				if d := time.Until(next); d < wait {
					wait = d
				}
			}
			if wait < 0 {
				wait = 0
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// RunModelWindowProbeNow 立即处理一轮到点的窗口，返回实际发出的探针数。
// 面板「立即探针」与测试用；正常路径由 StartModelWindowProbe 驱动。
func (s *Scheduler) RunModelWindowProbeNow() int {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return 0
	}
	return s.runWindowProbes(context.Background(), s.cfg.Pool.DueModelWindows(time.Now()))
}

// runWindowProbes 逐条处理窗口。返回实际发出的探针数（不含被跳过的）。
func (s *Scheduler) runWindowProbes(ctx context.Context, due []pool.ModelWindow) int {
	if len(due) > windowProbeBatchLimit {
		due = due[:windowProbeBatchLimit]
	}
	sent := 0
	for i, w := range due {
		if ctx.Err() != nil {
			return sent
		}
		if i > 0 {
			// 可取消的错开等待：ctx 取消（停机）时不必等满。
			select {
			case <-ctx.Done():
				return sent
			case <-time.After(windowProbeGap):
			}
		}
		if s.probeWindow(ctx, w) {
			sent++
		}
	}
	return sent
}

// probeWindow 处理单个窗口，返回是否真的发出了上游请求。
// 每条路径都会删记录或把到点时刻推后，循环不会空转。
func (s *Scheduler) probeWindow(ctx context.Context, w pool.ModelWindow) bool {
	p := s.cfg.Pool
	if why := p.WindowProbeBlocked(w.UID, w.Model); why != "" {
		// 不发：记录留着只会每轮都被取出来。真实请求照常会开窗，所以直接删。
		p.DropModelWindow(w.UID, w.Model)
		log.Printf("INFO: [window] %s 的 %s 不探针（%s），等真实请求开窗", shortUID(w.UID), w.Model, why)
		return false
	}

	opened, limited, resetAt, detail := s.fireWindowProbe(ctx, w)
	if ctx.Err() != nil {
		// 停机打断的探针不代表任何上游结论：记录原样留给下次启动。
		return true
	}
	now := time.Now()
	switch {
	case opened:
		// 探针通了：这条额度线已经重置。清 6004 冷却 + 删窗口记录。
		//
		// 清冷却的对齐点是**这条窗口自己的 ResetAt**，不是现在：探针证明的是「这条
		// 窗口对应的那轮限额已经过去」，只有 ResetAt 不晚于它的冷却才该被清。探针
		// 期间若又撞上更新一轮的限额（ResetAt 更晚），那条是上游刚给的墙钟，不能被
		// 这次成功抹掉。
		cleared := p.ClearQuotaCooldown(w.UID, w.Model, w.ResetAt)
		p.DropModelWindow(w.UID, w.Model)
		log.Printf("INFO: [window] %s 的 %s 已开始新一轮计时（探针成功，清冷却=%v）",
			shortUID(w.UID), w.Model, cleared)
	case limited && resetAt.After(now.Add(windowProbeFreshMin)):
		// 上游给了更晚的重置点：这条线确实进入了新的等待期，换成新墙钟、计数归零。
		p.RecordWindowProbe(w.UID, w.Model, resetAt, 0, detail, now)
		log.Printf("INFO: [window] %s 的 %s 仍在限流，下一轮重置 %s",
			shortUID(w.UID), w.Model, resetAt.Format("2006-01-02 15:04:05"))
	case limited && w.Attempts+1 < windowProbeMaxAttempts:
		p.RecordWindowProbe(w.UID, w.Model, now.Add(windowProbeRetryDelay), w.Attempts+1, detail, now)
		log.Printf("INFO: [window] %s 的 %s 尚未重置（%s），%s 后重试 (%d/%d)",
			shortUID(w.UID), w.Model, orNone(detail), windowProbeRetryDelay, w.Attempts+1, windowProbeMaxAttempts)
	case limited:
		p.DropModelWindow(w.UID, w.Model)
		log.Printf("WARN: [window] %s 的 %s 连续 %d 次未重置，放弃（下一次真实 429 会重新登记）",
			shortUID(w.UID), w.Model, windowProbeMaxAttempts)
	default:
		p.DropModelWindow(w.UID, w.Model)
		log.Printf("WARN: [window] %s 的 %s 探针失败（%s），不再重试，等真实请求开窗",
			shortUID(w.UID), w.Model, orNone(detail))
	}
	return true
}

// fireWindowProbe 发一次最小请求。
//
// 返回 (opened, limited, resetAt, detail)：
//   - opened=true   首个有效帧是正常内容，该模型的额度窗口已重置；
//   - limited=true  上游仍按限流拒绝（HTTP 429、报文带重置时刻或模型级限流码，含 200 流里
//     的错误帧），resetAt 是报文里的重置时刻（可能为零）；
//   - detail        失败原因（日志与窗口记录用）。
func (s *Scheduler) fireWindowProbe(ctx context.Context, w pool.ModelWindow) (opened, limited bool, resetAt time.Time, detail string) {
	a := s.cfg.Pool.AuthByUID(w.UID)
	if a == nil {
		return false, false, time.Time{}, "账号已不在池中"
	}
	body, err := windowProbeBody(w.Model)
	if err != nil {
		return false, false, time.Time{}, "构造请求失败: " + err.Error()
	}
	pctx, cancel := context.WithTimeout(ctx, windowProbeTimeout)
	defer cancel()
	// AgentType=auxiliary：这是一次内部维护请求，不是用户主对话（口径同
	// web_search 的探测轮）。上游看到的仍是一个正常的 chat 请求。
	rc, status, respBody, err := s.cfg.Upstream.ChatStreamContext(pctx, a, body, "",
		upstream.ChatMeta{AgentType: upstream.AgentTypeAuxiliary})
	// ≥400 时 ChatStreamContext 同时返回 status、原始 respBody 和已分类的 *Error：限流判定按
	// 状态码与原始报文——*Error.Msg 只留前 200 字节，重置时刻排在后面就会被截掉，
	// 不能因为 err 非空就当成「其它错误」放弃。
	var ue *upstream.Error
	if errors.As(err, &ue) && status == 0 {
		status = ue.Status
	}
	if err != nil && status < 400 {
		return false, false, time.Time{}, clip(err.Error())
	}
	if status >= 400 {
		if len(respBody) == 0 && ue != nil {
			respBody = []byte(ue.Msg)
		}
		reset, ok := upstream.ParseRateReset(string(respBody))
		return false, status == 429 || ok, reset, "HTTP " + strconv.Itoa(status) + ": " + clip(string(respBody))
	}
	if rc == nil {
		return true, false, time.Time{}, ""
	}
	defer rc.Close()
	return scanProbeStream(rc)
}

// scanProbeStream 读到第一个有内容的帧为止：上游常把限流与拦截塞进 HTTP 200 的 SSE 帧里，
// 只看状态码会把「仍在限流」当成打通，清掉冷却、钉错窗口起点。
func scanProbeStream(r io.Reader) (opened, limited bool, resetAt time.Time, detail string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 16<<10), 1<<20)
	for sc.Scan() {
		payload, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		_, isErr := obj["error"]
		_, msg, embedded := upstream.StreamErrorFrame(obj)
		if isErr || embedded {
			if msg == "" {
				msg = payload
			}
			reset, ok := upstream.ParseRateReset(payload)
			return false, ok || upstream.IsModelRateLimit(payload), reset, clip(msg)
		}
		if _, has := obj["choices"]; has {
			return true, false, time.Time{}, ""
		}
	}
	if err := sc.Err(); err != nil {
		return false, false, time.Time{}, clip(err.Error())
	}
	return false, false, time.Time{}, "流结束前没有有效帧"
}

// shortUID 日志里的账号称呼：前 8 位，不落完整 UID（同 dump.go 口径）。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	if uid == "" {
		return "?"
	}
	return uid
}

func orNone(s string) string {
	if s == "" {
		return "无详情"
	}
	return s
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
