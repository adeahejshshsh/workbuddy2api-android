// probe.go 台账里没有的任务 id 怎么查：有界探测（视频 / 3D 共用）。
//
// 任务只对提交它的账号可见，而上游任务 id 反推不出账号。台账命中时直接问那个账号；台账没有
// （进程重启、超出容量/时长、别处提交、或客户端乱填）时，唯一的办法是拿账号去试。早先的实现
// 对两个域的**每个**可用账号各打一次，而且查不到不留记忆——任何持有有效 key 的客户端换着花样
// 填 id，就能把请求放大 N 倍打满整池，触发上游 429。现在收紧成：
//
//  1. 负缓存：查不到的 id 在 cfg.retry 内直接回「跳过」，不碰上游；条目保留 cfg.keep，
//     里面带游标，下一次放行时从上次停下的地方接着试，多次重试能轮遍整池而不是总问同几个号；
//  2. 每次最多问 cfg.max 个账号；
//  3. 全局限速：每分钟最多 cfg.budget 次探测请求，用尽则回「跳过」；
//  4. 探测遇到 404 / 403 / 400（不是这个账号的任务）才换下一个，其余错误（429 / 5xx / 网络）
//     立即停手并暂停全局探测 cfg.pause。**探测失败从不调用 applyErrorPolicy**：
//     探测是网关自己发起的盲查，失败说明不了账号的健康。
//
// 找到之后由调用方收养进台账（已终态的标已记账、还在跑的保持未记账），后续查询直接走台账命中路径。
package server

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// errProbeSkipped 探测被负缓存 / 限速 / 暂停挡掉，没有碰上游。
var errProbeSkipped = errors.New("probe skipped")

// errProbeNotFound 探测过的账号都说不是自己的任务。
var errProbeNotFound = errors.New("task not found")

// errProbeNotMine try 的返回值：这个账号看不见该任务，换下一个（不是错误，不计入健康度）。
var errProbeNotMine = errors.New("not this account's task")

// probeConfig 一个探测器的参数（每个功能一套，互不抢占额度）。
type probeConfig struct {
	max    int           // 单次探测最多问几个账号
	budget int           // 每分钟全局最多几次探测请求
	retry  time.Duration // 同一 id 查不到之后的静默期
	keep   time.Duration // 负缓存条目（含游标）的保留时长
	pause  time.Duration // 探测撞上限流 / 故障后的全局暂停
	missed int           // 负缓存容量上限（防乱填 id 撑内存）
	label  string        // 日志前缀
}

type probeMiss struct {
	retryAt, expire time.Time
	cursor          int
}

type taskProber struct {
	cfg probeConfig

	mu          sync.Mutex
	miss        map[string]*probeMiss
	winStart    time.Time
	winCount    int
	pausedUntil time.Time
}

func newTaskProber(cfg probeConfig) taskProber {
	return taskProber{cfg: cfg, miss: map[string]*probeMiss{}}
}

// begin 判断能否探测 id，返回游标。被负缓存或暂停挡住返回 false。
func (p *taskProber) begin(id string, now time.Time) (cursor int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Before(p.pausedUntil) {
		return 0, false
	}
	if m := p.miss[id]; m != nil {
		if now.Before(m.retryAt) {
			return 0, false
		}
		return m.cursor, true
	}
	return 0, true
}

// reserve 从全局每分钟额度里取最多 n 次，返回实际取到的数量。
func (p *taskProber) reserve(n int, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.winStart) >= time.Minute {
		p.winStart, p.winCount = now, 0
	}
	if left := p.cfg.budget - p.winCount; n > left {
		n = left
	}
	if n < 0 {
		n = 0
	}
	p.winCount += n
	return n
}

// missed 记一次没找到：进入静默期，游标前进 advanced 步。
func (p *taskProber) missed(id string, advanced int, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.miss == nil {
		p.miss = map[string]*probeMiss{}
	}
	m := p.miss[id]
	if m == nil {
		if len(p.miss) >= p.cfg.missed {
			for k, v := range p.miss { // 先清过期的；仍满就随手丢一条，容量是硬上限
				if now.After(v.expire) {
					delete(p.miss, k)
				}
			}
			for k := range p.miss {
				if len(p.miss) < p.cfg.missed {
					break
				}
				delete(p.miss, k)
			}
		}
		m = &probeMiss{}
		p.miss[id] = m
	}
	m.cursor += advanced
	m.retryAt, m.expire = now.Add(p.cfg.retry), now.Add(p.cfg.keep)
}

func (p *taskProber) found(id string) {
	p.mu.Lock()
	delete(p.miss, id)
	p.mu.Unlock()
}

func (p *taskProber) pause(now time.Time) {
	p.mu.Lock()
	p.pausedUntil = now.Add(p.cfg.pause)
	p.mu.Unlock()
}

// probeCandidates 探测候选账号：未禁用的全部账号，按 UID 排序。
func (h *Handler) probeCandidates() []string {
	var uids []string
	for _, st := range h.cfg.Pool.List() { // List 已按 UID 排序
		if !st.Disabled {
			uids = append(uids, st.UID)
		}
	}
	return uids
}

// probeAccounts 有界探测：按游标依次问账号，try 返回 errProbeNotMine 就换下一个。
//
// 返回命中的账号；全部问完都不是则返回 errProbeNotFound；被负缓存 / 额度 / 暂停挡住返回
// errProbeSkipped；429 / 5xx / 网络错误原样返回（并暂停全局探测）。
func (h *Handler) probeAccounts(p *taskProber, id string, try func(a *auth.Auth) error) (*auth.Auth, error) {
	now := time.Now()
	cursor, ok := p.begin(id, now)
	if !ok {
		return nil, errProbeSkipped
	}
	// 候选 = 两域所有未禁用的账号（按 UID 排序，游标才有稳定含义）；域未知，所以不分域。
	// 不用 AvailableUIDsForRealm：它会剔除冷却中 / 在途占满的账号，名单随时抖动，游标就失去意义，
	// 而提交任务的账号恰好忙着（刚提交过）的可能性并不低。
	uids := h.probeCandidates()
	if len(uids) == 0 {
		return nil, errProbeNotFound
	}
	n := p.reserve(min(p.cfg.max, len(uids)), now)
	if n == 0 {
		return nil, errProbeSkipped
	}
	tried := 0
	for i := 0; i < n; i++ {
		tried++
		a := h.cfg.Pool.AuthByUID(uids[(cursor+i)%len(uids)])
		if a == nil {
			continue
		}
		err := try(a)
		if err == nil {
			p.found(id)
			return a, nil
		}
		if errors.Is(err, errProbeNotMine) {
			continue // 不是这个账号的任务，换下一个
		}
		// 限流 / 5xx / 网络：盲查失败说明不了账号健康，不罚号；暂停全局探测，别在故障期火上浇油。
		p.pause(time.Now())
		p.missed(id, tried, time.Now())
		log.Printf("%s task=%q: 探测被上游拒绝，暂停探测 %s：%v", p.cfg.label, id, p.cfg.pause, err)
		return nil, err
	}
	p.missed(id, tried, time.Now())
	return nil, errProbeNotFound
}

// probeNotMine 把「这个账号看不见该任务」的上游错误识别出来。
func probeNotMine(err error) bool {
	var ue *upstream.Error
	return errors.As(err, &ue) && (ue.Status == http.StatusNotFound ||
		ue.Status == http.StatusForbidden || ue.Status == http.StatusBadRequest)
}

// probeAdoptLog 收养成功时的统一日志（不含任何凭证）。
func probeAdoptLog(label, id string, a *auth.Auth) {
	log.Printf("%s task=%q: 台账里没有，探测命中账号 %s（已收养）", label, id, logfmt.Label(a.UID, a.Nickname))
}
