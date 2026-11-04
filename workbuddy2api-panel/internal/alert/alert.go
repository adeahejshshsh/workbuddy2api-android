// Package alert 账号池可用性阈值告警。Enabled=false 时 Run 直接返回。
package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
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

type Monitor struct {
	cfg       Config
	src       Source
	rules     []*rule
	client    *http.Client
	startedAt time.Time
	mu        sync.Mutex
	stop      chan struct{}
	stopOnce  sync.Once
}

func New(cfg Config, src Source) *Monitor {
	cfg.normalize()
	return &Monitor{
		cfg: cfg, src: src, rules: newRules(cfg), startedAt: time.Now(),
		client: &http.Client{
			Timeout: cfg.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 2 {
					return fmt.Errorf("too many redirects")
				}
				if req.URL.Host != via[0].URL.Host {
					return fmt.Errorf("cross-host redirect refused")
				}
				return nil
			},
		},
	}
}

func (m *Monitor) Run(ctx context.Context) {
	if !m.cfg.Enabled || m.src == nil {
		return
	}
	m.mu.Lock()
	if m.stop != nil {
		m.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	m.stop = stop
	m.mu.Unlock()
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	m.Tick(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			m.Tick(time.Now())
		}
	}
}

func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.stop != nil {
			close(m.stop)
			m.stop = nil
		}
	})
}

func (m *Monitor) Tick(now time.Time) {
	if !m.cfg.Enabled || m.src == nil {
		return
	}
	if now.Sub(m.startedAt) < m.cfg.StartupGrace {
		return
	}
	for _, r := range m.rules {
		ev, value, threshold, msg := r.step(now, m.src, m.cfg.ForTicks, m.cfg.ClearTicks)
		switch ev {
		case eventAlert:
			m.send("alert", r, now, value, threshold, msg)
		case eventResolve:
			if m.cfg.SendResolve {
				m.send("resolve", r, now, value, threshold, msg)
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
		Message: message, FiredAt: time.Now(), Snapshot: m.snapshot(),
	}, "notify")
}

// post 发 payload 到 webhook（HMAC 签名同规则事件；失败只记日志，不重试）。
func (m *Monitor) post(p Payload, tag string) {
	body, err := json.Marshal(p)
	if err != nil {
		log.Printf("WARN: [alert] marshal payload rule=%s: %v", tag, err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, m.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("WARN: [alert] build request rule=%s: %v", tag, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "workbuddy2api-alert/1")
	if m.cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(m.cfg.Secret))
		mac.Write(body)
		req.Header.Set("X-WB2A-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		log.Printf("WARN: [alert] webhook post failed rule=%s: %v", tag, err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("WARN: [alert] webhook returned %d rule=%s", resp.StatusCode, tag)
	}
}
