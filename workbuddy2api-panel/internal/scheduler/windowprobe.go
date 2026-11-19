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
	// windowProbeMinWait 巡检等待的**下限**（审查 C7-040）。
	//
	// NextModelWindowDue 与 DueModelWindows 是两个独立判据：前者按「最早到点时刻」
	// 返回，后者按「已到点」过滤。两者口径一旦出现偏差（例如 DueModelWindows 还
	// 要过 attempts 推后、或 pool 侧状态被并发改），NextModelWindowDue 就可能返回
	// 一个**已经过去**的时刻 —— 原来的 `if wait < 0 { wait = 0 }` 会把它压成 0，
	// 于是循环「查一次 → 立即重查」变成 100% CPU 空转，且每轮都扫一遍全池窗口。
	// 500ms 的代价在秒级事件上无感，却把最坏情况从「忙等」压成 2 次/秒。
	windowProbeMinWait = 500 * time.Millisecond
	// windowProbeManualBudget 「立即探针」无 ctx 入口的总预算（审查 C7-039）。
	//
	// 无 ctx 的旧入口此前传 context.Background()：一次点击最长挂 8×60s ≈ 8 分钟，
	// 客户端/反代早就超时了，而停机时也没人能打断它。保留该入口（调用方零改动）
	// 但给它一个**硬预算**：超过就收摊，剩余窗口留给下一轮巡检。
	windowProbeManualBudget = 5 * time.Minute
	// windowProbeBusyBackoff 一轮巡检「一条都没发出去」时的硬退避（W4-02）。
	//
	// 原来的循环在 due 非空时处理完立刻 continue 重查；只要 probeWindow 全部返回
	// false（族锁被占、或闸门拦下后记录已删），DueModelWindows 会因为 ResetAt 没变
	// 而反复命中同一批窗口 —— 面板「立即探针」持锁最长 60s，后台就紧凑空转 60s
	// （每轮 RLock + 全表遍历 + sort）。2s 对秒级窗口事件无感，却把最坏情况压到
	// 0.5 次/秒；窗口本身还会被族锁失败路径推后 15s，正常情况下退避根本用不上。
	windowProbeBusyBackoff = 2 * time.Second
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
//
// 启动幂等（审查 C7-041）：此前每次调用都 `go func()`，重复装配（main 里装配一次、
// 面板热重载再装一次）会让两个循环同时跑同一批窗口 —— 每次探针都是真实计费请求，
// 而 probeWindow 的族锁只能让其中一个「失败返回」，等于白扫一轮全池窗口。
func (s *Scheduler) StartModelWindowProbe(ctx context.Context) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	if !s.probeStarted.CompareAndSwap(false, true) {
		log.Printf("scheduler: 窗口探针循环已启动，忽略重复装配")
		return
	}
	s.probeLoops.Add(1)
	go func() {
		for {
			due := s.cfg.Pool.DueModelWindows(time.Now())
			if len(due) > 0 {
				sent := s.runWindowProbes(ctx, due)
				if ctx.Err() != nil {
					return
				}
				if sent == 0 {
					// W4-02：本轮一条探针都没发出去（全部被族锁/闸门挡下）。此前直接
					// continue，而窗口的 ResetAt 没变 —— 同一批窗口被立刻再取出来，
					// 紧凑空转整整一个「立即探针」的持锁时长（最长 60s）。硬退避一段。
					if !s.waitProbeBackoff(ctx) {
						return
					}
				}
				// 一轮处理完立刻回到循环顶重判（可能还有待处理窗口）。
				continue
			}
			next, ok := s.cfg.Pool.NextModelWindowDue()
			// 等待时长统一走 windowProbeWait：把「下限」与「空闲兜底」收在一处，
			// 免得下次改动又漏掉其中一条（审查 C7-040）。
			timer := time.NewTimer(windowProbeWait(time.Now(), ok, next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// windowProbeWait 空闲期该睡多久：最近的到点时刻与空闲兜底取小，再夹一个下限。
//
// 单列成函数是为了能直接被用例钉住（审查 C7-040）：真正的坑不是「算出多少」，
// 而是「算出一个 <= 0 的值」——那会让巡检循环退化成忙等。
func windowProbeWait(now time.Time, hasNext bool, next time.Time) time.Duration {
	wait := windowProbeIdle
	if hasNext {
		if d := next.Sub(now); d < wait {
			wait = d
		}
	}
	if wait < windowProbeMinWait {
		// 到点时刻已过去（或近到不值得再睡）：睡一个下限，下一轮立刻会命中
		// DueModelWindows，不会漏探；也不会空转。
		wait = windowProbeMinWait
	}
	return wait
}

// RunModelWindowProbeNow 立即处理一轮到点的窗口，返回实际发出的探针数。
//
// 无 ctx 的历史入口（面板/外部脚本可能直接调）：语义上等价于「一次点击」，
// 所以内部给一个 windowProbeManualBudget 的硬预算，不再用 Background 无限挂
// （审查 C7-039）。需要随请求生命周期取消的调用方用 RunModelWindowProbeNowCtx。
func (s *Scheduler) RunModelWindowProbeNow() int {
	ctx, cancel := context.WithTimeout(context.Background(), windowProbeManualBudget)
	defer cancel()
	return s.RunModelWindowProbeNowCtx(ctx)
}

// RunModelWindowProbeNowCtx 带 ctx 的「立即探针」：面板 handler 传 r.Context()，
// 客户端断开或反代超时即取消；停机时传 Scheduler 的 ctx（审查 C7-039）。
func (s *Scheduler) RunModelWindowProbeNowCtx(ctx context.Context) int {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.runWindowProbes(ctx, s.cfg.Pool.DueModelWindows(time.Now()))
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
			// 用 NewTimer 而不是 time.After（审查 C7-044）：ctx 先完成时 time.After
			// 的 timer 会挂到触发为止才回收，一次巡检白留 400ms 级定时器。
			gap := time.NewTimer(windowProbeGap)
			select {
			case <-ctx.Done():
				gap.Stop()
				return sent
			case <-gap.C:
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
	// 账号级族锁（审查 E4）：后台循环与面板「立即探针」会同时打同一个窗口，
	// 而每次探针都是**真实计费**的上游请求。
	if !s.TryBeginAccountTask(w.UID, TaskWindowProbe) {
		// W4-02：这是唯一既不 DropModelWindow 也不 RecordWindowProbe 的返回路径。
		// ResetAt 不推后 → DueModelWindows 下一轮仍命中同一条，Run 循环立刻重查
		// 就是紧凑空转。这里把窗口推到常规重试时刻（attempts 不变：这次一个请求
		// 都没发出，不能算作探针失败，否则会被白算满 20 次而提前放弃）。
		now := time.Now()
		p := s.cfg.Pool
		if p != nil {
			p.RecordWindowProbe(w.UID, w.Model, now.Add(windowProbeRetryDelay), w.Attempts,
				"同账号同族已有探针在跑（族锁被占用），稍后重试", now)
		}
		s.noteWindowProbeLockSkip(w)
		return false
	}
	defer s.EndAccountTask(w.UID, TaskWindowProbe)
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
		// 「已开窗」的判据必须是**有内容**（审查 C7-043）：此前只要帧里有 choices 就
		// 判开窗，而上游的保活/心跳帧正是 `{"choices":[{"delta":{}}]}`（空 delta）。
		// 那种帧被当成开窗 → 清掉 6004 冷却 + 删窗口记录，可额度其实没回来，下一条
		// 真实请求继续 429，且这次不再有人盯着那条窗口了。要求 delta.content 非空或
		// finish_reason 出现：role-only 首帧（OpenAI 兼容流常见）会继续往下读，
		// 紧跟的内容帧照样能判开窗。
		if ch, has := obj["choices"]; has {
			if probeFrameHasContent(ch) {
				return true, false, time.Time{}, ""
			}
			continue
		}
	}
	if err := sc.Err(); err != nil {
		// 单帧超限 ≠ 探针失败（审查 C7-042）：Scanner 的 1MB 上限被超长帧撞到时，
		// 此前按「其它错误」处理 → 直接删窗口记录，该窗口当天不再尝试。这里归到
		// 「仍未知」这一档（limited=true + 空 resetAt），走 probeWindow 的 15s 重试
		// 分支：既不清冷却、也不丢窗口，等下一帧正常了再判。
		if errors.Is(err, bufio.ErrTooLong) {
			return false, true, time.Time{}, "响应帧超过 1MB，无法判定（按未重置重试）"
		}
		return false, false, time.Time{}, clip(err.Error())
	}
	return false, false, time.Time{}, "流结束前没有有效帧"
}

// probeFrameHasContent 判断一帧 choices 是否代表「模型真的开始产出」（审查 C7-043）。
func probeFrameHasContent(v any) bool {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return false
	}
	for _, it := range arr {
		choice, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			return true
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if s, ok := delta["content"].(string); ok && s != "" {
			return true
		}
	}
	return false
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
	// 按 rune 截断（审查 P-25）：按字节切会把中文报文切成乱码（`s[:200]` 可能落在
	// 多字节字符中间）。同仓 logfmt.Truncate 有同款实现，这里保持一致。
	const maxRunes = 200
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes])
	}
	return s
}

// noteWindowProbeLockSkip 记录一次「因族锁被占用而跳过」并节流告警（W4-02）。
//
// 为什么要有失败计数：跳过本身是正常互斥（手动探针就该赢过后台），但**持续**跳过
// 说明锁被长期占用（面板点了一次 60s 的探针，或某条路径忘了 EndAccountTask）——
// 后者会让这条窗口永远等不到探针。首跳打一条、之后每 windowProbeSkipLogEvery 次
// 一条，既不刷屏也不静默。
const windowProbeSkipLogEvery = 10

func (s *Scheduler) noteWindowProbeLockSkip(w pool.ModelWindow) {
	n := s.probeLockSkips.Add(1)
	if n == 1 || n%windowProbeSkipLogEvery == 0 {
		log.Printf("WARN: [window] %s 的 %s 因族锁被占用跳过 %d 次（推后 %s 重试；检查是否有探针长期持锁）",
			shortUID(w.UID), w.Model, n, windowProbeRetryDelay)
	}
}

// waitProbeBackoff 零发送后的硬退避（W4-02）。返回 false = ctx 已取消（调用方应退出）。
// 单列成方法是为了能被用例钉住：真正的风险是「这个退避被删掉/不被调用」，
// 而不是它睡多久。
func (s *Scheduler) waitProbeBackoff(ctx context.Context) bool {
	s.noteWindowProbeIdleRound()
	t := time.NewTimer(windowProbeBusyBackoff)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// noteWindowProbeIdleRound 记录一次「整轮零发送」的巡检（W4-02），节流告警。
func (s *Scheduler) noteWindowProbeIdleRound() {
	n := s.probeIdleRounds.Add(1)
	if n == 1 || n%windowProbeSkipLogEvery == 0 {
		log.Printf("WARN: [window] 本轮巡检未发出任何探针（全部被族锁/闸门跳过），硬退避 %s（累计 %d 轮）",
			windowProbeBusyBackoff, n)
	}
}
