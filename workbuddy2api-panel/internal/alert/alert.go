// Package alert 账号池可用性阈值告警。Enabled=false 时 Run 直接返回。
package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultInterval     = 30 * time.Second
	defaultTimeout      = 5 * time.Second
	defaultForTicks     = 2
	defaultClearTicks   = 2
	defaultStartupGrace = 30 * time.Second
	defaultRecover      = 1
	defaultServiceName  = "workbuddy2api"

	// postQueueCap 异步发送队列容量（审查 W2-C2-12）。
	// 取 64：一轮最多 4 条规则，积压 16 轮足够吸收一次网络抖动；满了就丢并计数，
	// 绝不让 ticker 协程在 webhook 上排队（那样采集与其它规则判定全部停摆）。
	postQueueCap = 64
	// maxDrainBytes 响应体最多读这么多再关（审查 W2-C2-14）。
	maxDrainBytes = 64 << 10
	// notifyMaxBytes Notify 消息截断上限（审查 W2-C2-15）。
	notifyMaxBytes = 2048
)

type Health struct {
	Total, Healthy, Cooling, Disabled, Breaker, Degraded, InFlight int
}

type Source interface {
	Health(realm string) Health
	WAFActive() bool
}

type Config struct {
	Enabled          bool
	WebhookURL       string
	Secret           string
	Interval         time.Duration
	Timeout          time.Duration
	StartupGrace     time.Duration
	MinHealthyCN     int
	MinHealthyGlobal int
	RecoverHealthy   int
	BreakerThreshold int
	ForTicks         int
	ClearTicks       int
	SendResolve      bool
	ServiceName      string

	// AllowInsecureHTTP 允许 http:// 明文 webhook 发到**非回环**主机（审查 W2-C2-08）。
	//
	// 为什么默认关：webhook URL 里通常就带着凭据（Slack/钉钉/企微的 token 在 path 或
	// query 里），明文 http 等于把它摆到链路上。回环（127.0.0.1/::1）例外放行——本机
	// 采集器与测试都是这个形态，且不出网卡。
	AllowInsecureHTTP bool
	// AllowPrivateHost 允许 POST 到私网/链路本地地址（审查 W2-C2-08）。
	//
	// 为什么默认关：配置面一旦被污染（面板/配置同步/环境变量注入），网关就会替攻击者
	// POST 到内网管理口或云元数据（169.254.169.254），形成 SSRF + 数据外带通道。
	// 自建内网 collector 的部署显式打开即可。
	AllowPrivateHost bool
}

func (c *Config) normalize() {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.StartupGrace <= 0 {
		c.StartupGrace = defaultStartupGrace
	}
	if c.ForTicks <= 0 {
		c.ForTicks = defaultForTicks
	}
	if c.ClearTicks <= 0 {
		c.ClearTicks = defaultClearTicks
	}
	if c.RecoverHealthy <= 0 {
		c.RecoverHealthy = defaultRecover
	}
	if c.ServiceName == "" {
		c.ServiceName = defaultServiceName
	}
}

type ruleState int

const (
	stateOK ruleState = iota
	statePending
	stateFiring
)

type rule struct {
	id, severity, realm string
	sample              func(Source) (value float64, bad, recovered bool, threshold float64, msg string)
	state               ruleState
	badTicks            int
	clearCount          int
	since               time.Time
}

type event int

const (
	eventNone event = iota
	eventAlert
	eventResolve
)

func (r *rule) step(now time.Time, src Source, forTicks, clearTicks int) (event, float64, float64, string) {
	v, bad, recovered, th, msg := r.sample(src)
	switch r.state {
	case stateOK:
		if !bad {
			return eventNone, 0, 0, ""
		}
		r.state = statePending
		r.badTicks = 0
		fallthrough
	case statePending:
		if !bad {
			r.state = stateOK
			r.badTicks = 0
			return eventNone, 0, 0, ""
		}
		r.badTicks++
		if r.badTicks >= forTicks {
			r.state = stateFiring
			r.since = now
			r.clearCount = 0
			return eventAlert, v, th, msg
		}
	case stateFiring:
		if recovered {
			r.clearCount++
			if r.clearCount >= clearTicks {
				r.state = stateOK
				r.badTicks = 0
				r.clearCount = 0
				return eventResolve, v, th, msg
			}
		} else {
			r.clearCount = 0
		}
	}
	return eventNone, 0, 0, ""
}

// prefill 在启动宽限期内采样，但**只记状态不发送**（审查 W2-C2-16）。
//
// 为什么：原先 grace 内 Tick 直接 return，规则一个 tick 都不计；若进程一起来就是坏的
// （例如账号目录没挂上、上游全 401），告警时刻 = grace + forTicks*interval，启动期故障
// 被整段平滑掉。这里把「已经坏了」的事实提前记进状态机（badTicks 记到 forTicks-1），
// 宽限期一结束的第一次 Tick 就立刻告警，且仍然不会在启动抖动时把人叫起来。
func (r *rule) prefill(src Source, forTicks int) {
	_, bad, _, _, _ := r.sample(src)
	if !bad {
		r.state, r.badTicks, r.clearCount = stateOK, 0, 0
		return
	}
	r.state, r.badTicks = statePending, forTicks-1
	if r.badTicks < 0 {
		r.badTicks = 0
	}
}

func newRules(cfg Config) []*rule {
	var rs []*rule
	for _, rc := range []struct {
		realm string
		min   int
	}{{"cn", cfg.MinHealthyCN}, {"global", cfg.MinHealthyGlobal}} {
		if rc.min <= 0 {
			continue
		}
		realm, min := rc.realm, rc.min
		rs = append(rs, &rule{
			id: "realm_healthy_low", severity: "critical", realm: realm,
			sample: func(src Source) (float64, bool, bool, float64, string) {
				v := float64(src.Health(realm).Healthy)
				bad := v < float64(min)
				return v, bad, v >= float64(cfg.RecoverHealthy), float64(min),
					fmt.Sprintf("%s realm healthy accounts (%d) below threshold (%d)", realm, int(v), min)
			},
		})
	}
	if cfg.BreakerThreshold > 0 {
		th := cfg.BreakerThreshold
		rs = append(rs, &rule{
			id: "breaker_high", severity: "warning",
			sample: func(src Source) (float64, bool, bool, float64, string) {
				v := float64(src.Health("cn").Breaker + src.Health("global").Breaker)
				return v, v >= float64(th), v < float64(th), float64(th),
					fmt.Sprintf("accounts in circuit-breaker (%d) reached threshold (%d)", int(v), th)
			},
		})
	}
	rs = append(rs, &rule{
		id: "waf_ip_block", severity: "warning",
		sample: func(src Source) (float64, bool, bool, float64, string) {
			active := src.WAFActive()
			v := 0.0
			if active {
				v = 1
			}
			return v, active, !active, 1, "IP-level WAF block is active"
		},
	})
	return rs
}

type Snapshot struct {
	CN        RealmSnapshot `json:"cn"`
	Global    RealmSnapshot `json:"global"`
	WAFActive bool          `json:"waf_active"`
}

type RealmSnapshot struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Cooling  int `json:"cooling"`
	Disabled int `json:"disabled"`
	Breaker  int `json:"breaker"`
	// Degraded 连败降权中的账号数。alert.Health 一直有这个观测，但快照转换时
	// 被丢弃——webhook 接收方永远看不到「有一批号在连败降权」，只能等它恶化成
	// healthy 不足才被 realm_healthy_low 告警。
	Degraded int `json:"degraded"`
	InFlight int `json:"in_flight"`
}

type Payload struct {
	Version   int       `json:"version"`
	Event     string    `json:"event"`
	Service   string    `json:"service"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Realm     string    `json:"realm,omitempty"`
	Value     float64   `json:"value"`
	Threshold float64   `json:"threshold"`
	Message   string    `json:"message"`
	Since     time.Time `json:"since"`
	FiredAt   time.Time `json:"fired_at"`
	Snapshot  Snapshot  `json:"snapshot"`
}

// postJob 一条待发送的 webhook 消息（异步队列元素，审查 W2-C2-12）。
type postJob struct {
	payload Payload
	tag     string
}

type Monitor struct {
	cfg       Config
	src       Source
	rules     []*rule
	client    *http.Client
	startedAt time.Time
	mu        sync.Mutex

	// tickMu 串行化 Tick 的规则状态机（审查 W2-C2-11）。
	//
	// 为什么单独一把锁：Tick 是导出方法，将来被 HTTP handler / healthcheck 复用时，
	// 会与 Run 的 ticker 协程同时进入 r.step，读写 rule.state/badTicks 构成数据竞争
	// （规则状态错乱 → 重复告警或漏告警）。锁只覆盖状态机，发送在锁外做，避免把
	// webhook 的 RTT 也算进临界区。
	tickMu sync.Mutex

	// stop 只创建一次，Stop 之后不再重建（审查 W2-C2-10）。
	//
	// 原实现把 m.stop 置 nil，下一次 Run 见 nil 又新建 channel，但 stopOnce 已经消耗
	// 掉了 —— 第二次 Run 起的 goroutine 永远无法被 Stop 关闭（热重载告警模块时泄漏
	// ticker 协程，重复发告警）。现在 stop 在 New 里建好、Stop 只关一次，用 running
	// 原子标志表示「是否在跑」；语义上 Stop 是终态，同一 Monitor 不再重启。
	stop     chan struct{}
	stopOnce sync.Once
	running  atomic.Bool

	// queue 异步发送队列（审查 W2-C2-12）：只在 Run 起来后启用。
	queue   chan postJob
	dropped atomic.Uint64

	// blocked 启动校验失败的 webhook 目标（审查 W2-C2-08）：非空即拒发，
	// 并在日志里说明原因（只记 host，不带 path/query 里的凭据）。
	blocked   error
	blockOnce sync.Once
}

func New(cfg Config, src Source) *Monitor {
	cfg.normalize()
	m := &Monitor{
		cfg: cfg, src: src, rules: newRules(cfg), startedAt: time.Now(),
		stop:  make(chan struct{}),
		queue: make(chan postJob, postQueueCap),
	}
	// 没配 URL 就当作未启用（审查 W2-C2-13）：否则规则状态机会照常推进到 Firing
	// （事件已被"消费"），post 在 NewRequest 处失败只记日志 → 告警永久丢失，
	// 面板上却显示「已触发」。宁可显式关掉并告警。
	if cfg.Enabled && strings.TrimSpace(cfg.WebhookURL) == "" {
		log.Printf("WARN: [alert] alerting.enabled=true 但 webhook_url 为空，告警通道已关闭")
		m.cfg.Enabled = false
	}
	if m.cfg.Enabled {
		if err := validateWebhookURL(cfg.WebhookURL, cfg.AllowInsecureHTTP, cfg.AllowPrivateHost); err != nil {
			m.blocked = err
			log.Printf("WARN: [alert] webhook 目标被拒绝（%v），告警将不会发出；确认无误可设 "+
				"alerting.allow_private_host / allow_insecure_http 放开", err)
		}
	}
	m.client = &http.Client{
		Timeout:   cfg.Timeout,
		Transport: newGuardTransport(cfg.Timeout, cfg.AllowPrivateHost),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 2 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("cross-host redirect refused")
			}
			return nil
		},
	}
	return m
}

// newGuardTransport 在**连接建立时**再校验一次对端地址（审查 W2-C2-08）。
//
// 为什么不能只校验启动时的 URL 字符串：域名可以在解析阶段指向内网/元数据地址
// （DNS rebinding），字符串校验看不出 127.0.0.1 与 169.254.169.254 的差别。
// Control 拿到的是已解析的 sockaddr，正好是最后一刻能拦住的位置。
func newGuardTransport(timeout time.Duration, allowPrivate bool) *http.Transport {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	out := tr.Clone()
	dialer := &net.Dialer{Timeout: timeout}
	if !allowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return nil
			}
			if bad := blockedAddr(host); bad != "" {
				return fmt.Errorf("webhook 目标 %s 属于受限地址段（%s）", host, bad)
			}
			return nil
		}
	}
	out.DialContext = dialer.DialContext
	return out
}

// blockedAddr 判断地址是否属于「不该被告警通道访问」的段，返回原因（空 = 放行）。
//
// 回环**放行**：本机采集器与测试都是 127.0.0.1，且不出网卡；真正的 SSRF 目标是
// 云元数据（169.254.0.0/16）与私网管理口（10/8、172.16/12、192.168/16）。
func blockedAddr(host string) string {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// 解析不到 IP（域名未解析/DNS 失败）：交给拨号器按普通错误处理，这里不拦。
		return ""
	}
	switch {
	case ip.IsUnspecified():
		return "unspecified"
	case ip.IsLoopback():
		return ""
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "link-local（含云元数据 169.254.169.254）"
	case ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "multicast"
	case ip.IsPrivate():
		return "private"
	}
	return ""
}

// validateWebhookURL 启动期校验 webhook 目标（审查 W2-C2-08）。
func validateWebhookURL(raw string, allowInsecure, allowPrivate bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("webhook_url 为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhook_url 解析失败: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecure && !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("webhook_url 用了明文 http 且目标不是回环地址（凭据会随 query/path 上链路）")
		}
	default:
		return fmt.Errorf("webhook_url 只支持 http/https，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("webhook_url 缺主机")
	}
	if allowPrivate {
		return nil
	}
	// 常见元数据域名：即便解析不出 IP（离线环境）也要拦，字符串层面就能认出来。
	switch strings.ToLower(u.Hostname()) {
	case "metadata.google.internal", "metadata", "instance-data":
		return fmt.Errorf("webhook_url 指向云元数据主机 %q", u.Hostname())
	}
	if reason := blockedAddr(u.Hostname()); reason != "" {
		return fmt.Errorf("webhook_url 主机 %s 属于受限地址段（%s）", u.Hostname(), reason)
	}
	return nil
}

// isLoopbackHost 主机是否回环（localhost / 127.0.0.0/8 / ::1）。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (m *Monitor) Run(ctx context.Context) {
	if !m.cfg.Enabled || m.src == nil {
		return
	}
	// CAS 而不是「见 nil 就建」：同一 Monitor 重复 Run 不该起第二个 ticker 协程
	// （审查 W2-C2-10）。Stop 是终态，重启请新建 Monitor。
	if !m.running.CompareAndSwap(false, true) {
		return
	}
	defer m.running.Store(false)
	// 异步发送器随 Run 生命周期（审查 W2-C2-12）：ticker 只负责判定，投递在独立
	// 协程里做，一轮 4 条规则 × 5s 超时最坏 20s 的阻塞从此不再拖住采集。
	go m.sendLoop(ctx)

	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	m.Tick(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-t.C:
			m.Tick(time.Now())
		}
	}
}

// Stop 停止 Run 起的循环。可重复调用；Stop 之后同一 Monitor 不再重启（审查 W2-C2-10）。
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		close(m.stop)
	})
	m.running.Store(false)
}

// sendLoop 消费异步队列（审查 W2-C2-12）。
func (m *Monitor) sendLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case job := <-m.queue:
			m.deliver(job.payload, job.tag)
		}
	}
}

// Tick 采样一轮规则并按需推送告警。
func (m *Monitor) Tick(now time.Time) {
	if !m.cfg.Enabled || m.src == nil {
		return
	}
	// 启动宽限期：采样但只记状态、不发（审查 W2-C2-16）。
	if now.Sub(m.startedAt) < m.cfg.StartupGrace {
		m.tickMu.Lock()
		for _, r := range m.rules {
			r.prefill(m.src, m.cfg.ForTicks)
		}
		m.tickMu.Unlock()
		return
	}
	// 状态机在 tickMu 内跑完，发送放到锁外（审查 W2-C2-11 + W2-C2-12）：
	// 既避免并发 Tick 撕裂规则状态，也不把 webhook 的 RTT 算进临界区。
	type fired struct {
		ev               event
		r                *rule
		value, threshold float64
		msg              string
	}
	var out []fired
	m.tickMu.Lock()
	for _, r := range m.rules {
		ev, value, threshold, msg := r.step(now, m.src, m.cfg.ForTicks, m.cfg.ClearTicks)
		if ev == eventNone {
			continue
		}
		out = append(out, fired{ev: ev, r: r, value: value, threshold: threshold, msg: msg})
	}
	m.tickMu.Unlock()
	for _, f := range out {
		switch f.ev {
		case eventAlert:
			m.send("alert", f.r, now, f.value, f.threshold, f.msg)
		case eventResolve:
			if m.cfg.SendResolve {
				m.send("resolve", f.r, now, f.value, f.threshold, f.msg)
			}
		}
	}
}

func (m *Monitor) snapshot() Snapshot {
	to := func(h Health) RealmSnapshot {
		return RealmSnapshot{Total: h.Total, Healthy: h.Healthy, Cooling: h.Cooling, Disabled: h.Disabled, Breaker: h.Breaker, Degraded: h.Degraded, InFlight: h.InFlight}
	}
	return Snapshot{CN: to(m.src.Health("cn")), Global: to(m.src.Health("global")), WAFActive: m.src.WAFActive()}
}

func (m *Monitor) send(eventName string, r *rule, now time.Time, value, threshold float64, msg string) {
	m.post(Payload{
		Version: 1, Event: eventName, Service: m.cfg.ServiceName, Rule: r.id, Severity: r.severity,
		Realm: r.realm, Value: value, Threshold: threshold, Message: msg, Since: r.since, FiredAt: now,
		Snapshot: m.snapshot(),
	}, r.id)
}

// Notify 推送一条通知（无阈值语义的事件型消息，如排程轮次结果汇总）。
// 未启用或无 webhook 时静默——通知是旁路，不该影响调用方主流程。
func (m *Monitor) Notify(message string) {
	if !m.cfg.Enabled || m.cfg.WebhookURL == "" {
		return
	}
	m.post(Payload{
		Version: 1, Event: "notify", Service: m.cfg.ServiceName, Severity: "info",
		Message: sanitizeNotify(message), FiredAt: time.Now(), Snapshot: m.snapshot(),
	}, "notify")
}

// sanitizeNotify 截断并脱敏调用方传入的通知正文（审查 W2-C2-15）。
//
// 为什么在本包做：Notify 把调用方的任意字符串原样塞进 Payload 发到外部 webhook，
// 而调用方（排程汇总、目录监控）拼的消息里可能带 uid、错误原文甚至 token。告警群
// 就是第二份泄露面，这里兜住长度与常见凭据模式。
func sanitizeNotify(message string) string {
	out := message
	for _, pat := range []string{"token=", "key=", "secret=", "password=", "authorization: bearer "} {
		lower := strings.ToLower(out)
		var b strings.Builder
		pos := 0
		for {
			// 从 pos 往后找：命中后 pos 直接跳到值末尾，同一个 "token=" 不会被反复匹配
			// （否则 "token=***" 里还含 "token="，内层循环原地打转 —— 自测时真挂住过）。
			i := strings.Index(lower[pos:], pat)
			if i < 0 {
				b.WriteString(out[pos:])
				break
			}
			i += pos
			b.WriteString(out[pos : i+len(pat)])
			end := i + len(pat)
			for end < len(out) && !strings.ContainsRune(" \t\r\n&\"',;}", rune(out[end])) {
				end++
			}
			b.WriteString("***")
			pos = end
		}
		out = b.String()
	}
	if len(out) > notifyMaxBytes {
		out = out[:notifyMaxBytes] + "…(truncated)"
	}
	return out
}

// post 投递一条 payload：Run 起来时走有界队列，否则同步发送。
//
// 为什么要分两条路（审查 W2-C2-12）：ticker 协程里同步 POST 会让一轮判定被网络
// RTT 拖住（4 条规则 × 5s 超时 = 20s 停摆）；但直接调用 Tick/Notify 的场景
// （测试、一次性通知）需要「函数返回即已投递」的同步语义。用 running 标志区分。
func (m *Monitor) post(p Payload, tag string) {
	if m.blocked != nil {
		m.blockOnce.Do(func() {
			log.Printf("WARN: [alert] webhook 目标不可用（%v），本条及后续告警被丢弃", m.blocked)
		})
		return
	}
	if m.running.Load() {
		select {
		case m.queue <- postJob{payload: p, tag: tag}:
			return
		default:
			// 队列满：丢弃并计数（审查 W2-C2-12）。宁可丢一条告警，也不阻塞采集。
			n := m.dropped.Add(1)
			if n == 1 || n%100 == 0 {
				log.Printf("WARN: [alert] 发送队列已满，累计丢弃 %d 条（webhook 变慢/不可达？）", n)
			}
			return
		}
	}
	m.deliver(p, tag)
}

// deliver 真正发一条（同步），5xx/网络错误退避重试一次（审查 W2-C2-12）。
func (m *Monitor) deliver(p Payload, tag string) {
	if err := m.postOnce(p, tag); err != nil {
		// 瞬时抖动不该等于永久丢告警：一次 200ms 退避重试足够挡掉大部分网络抖动，
		// 又不至于把 ticker 拖长。
		time.Sleep(200 * time.Millisecond)
		_ = m.postOnce(p, tag)
	}
}

// postOnce 发一次；返回错误表示「值得重试」（网络错误或 5xx）。
func (m *Monitor) postOnce(p Payload, tag string) error {
	body, err := json.Marshal(p)
	if err != nil {
		log.Printf("WARN: [alert] marshal payload rule=%s: %v", tag, err)
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, m.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("WARN: [alert] build request rule=%s host=%s: %v", tag, webhookHost(m.cfg.WebhookURL), err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "workbuddy2api-alert/1")
	if m.cfg.Secret != "" {
		// 旧头保持原样（sha256=<hmac(body)>）：既有接收方与脚本不用改。
		mac := hmac.New(sha256.New, []byte(m.cfg.Secret))
		mac.Write(body)
		req.Header.Set("X-WB2A-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		// 新增带时间戳的头（审查 W2-C2-09）：只签 body 的旧头没有时效，抓到任意一条
		// 合法告警包就能无限重放（伪造「已恢复」压掉真告警）。V2 签的是 "t.<body>"，
		// 接收方校验 abs(now-t)<=5m 即可挡住重放；不校验 V2 的老接收方行为不变。
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac2 := hmac.New(sha256.New, []byte(m.cfg.Secret))
		mac2.Write([]byte(ts + "."))
		mac2.Write(body)
		req.Header.Set("X-WB2A-Signature-V2", "t="+ts+",v1="+hex.EncodeToString(mac2.Sum(nil)))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		// 只记 host：*url.Error 的 Error() 带完整 URL，path/query 里的 token 会直接进日志
		// （审查 A2）。webhook 常把凭据放在 query（?token=…）或 path 段里。
		log.Printf("WARN: [alert] webhook post failed rule=%s host=%s: %v", tag, webhookHost(m.cfg.WebhookURL), unwrapURLErr(err))
		return err
	}
	// 上限从 4KB 放宽到 64KB（审查 W2-C2-14）：4KB 读不完时连接无法复用，高告警频率下
	// 会堆积 TIME_WAIT。超过上限就直接关，不追求复用。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("WARN: [alert] webhook returned %d rule=%s host=%s", resp.StatusCode, tag, webhookHost(m.cfg.WebhookURL))
		if resp.StatusCode >= 500 {
			return fmt.Errorf("webhook 5xx: %d", resp.StatusCode)
		}
	}
	return nil
}

// webhookHost 只取 scheme://host，用于日志（不带 path/query 里的凭据）。
func webhookHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid)"
	}
	return u.Scheme + "://" + u.Host
}

// unwrapURLErr 把 *url.Error 拆成底层错误：默认 Error() 会拼出完整 URL。
func unwrapURLErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
