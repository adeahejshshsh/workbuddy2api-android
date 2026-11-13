// Package proxypool manages persistent account routes and independently probed proxies.
package proxypool

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
)

var (
	ErrNotFound = errors.New("proxy not found")
	ErrBound    = errors.New("proxy is bound to accounts; remove bindings first")
)

// Error identifies a route failure without exposing proxy credentials.
type Error struct {
	ProxyID string
	Reason  string
	cause   error
}

func (e *Error) Error() string { return "proxy route: " + e.Reason }
func (e *Error) Unwrap() error { return e.cause }

func IsError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

type Settings struct {
	URL              string `json:"url"`
	IntervalSeconds  int    `json:"interval_seconds"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	FailureThreshold int    `json:"failure_threshold"`
	SuccessThreshold int    `json:"success_threshold"`
}

func DefaultSettings() Settings {
	return Settings{"https://www.workbuddy.cn/", 60, 10, 3, 2}
}

func (s Settings) validate() error {
	u, err := url.Parse(s.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return errors.New("health URL must be an HTTP(S) URL without credentials or fragment")
	}
	if s.IntervalSeconds < 1 || s.IntervalSeconds > 86400 || s.TimeoutSeconds < 1 || s.TimeoutSeconds > 300 ||
		s.FailureThreshold < 1 || s.FailureThreshold > 100 || s.SuccessThreshold < 1 || s.SuccessThreshold > 100 {
		return errors.New("health interval must be 1-86400s, timeout 1-300s, thresholds 1-100")
	}
	return nil
}

type Binding struct {
	Mode    string `json:"mode"`
	ProxyID string `json:"proxy_id,omitempty"`
}

// realmKey 把 realm 归一化成配置键；只有 cn / global 合法，其余返回空（= 不匹配）。
func realmKey(realm string) string {
	if realm == "cn" || realm == "global" {
		return realm
	}
	return ""
}

type node struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

type disk struct {
	Nodes    []node             `json:"proxies"`
	Bindings map[string]Binding `json:"bindings"`
	// Realms realm 级默认出口（issue #39 P3）：cn/global → 绑定，账号没有显式
	// 绑定时取它。cn/global 的上游域名与网络环境不同，运维通常要两个出口。
	Realms   map[string]Binding `json:"realms,omitempty"`
	Settings Settings           `json:"health"`
}

type health struct {
	status      string
	failures    int
	successes   int
	checkedAt   time.Time
	attemptedAt time.Time
	latencyMS   int64
	lastError   string
}

type View struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Protocol    string    `json:"protocol"`
	PasswordSet bool      `json:"password_set"`
	Enabled     bool      `json:"enabled"`
	Status      string    `json:"status"`
	Checking    bool      `json:"checking"`
	LatencyMS   int64     `json:"latency_ms"`
	CheckedAt   time.Time `json:"checked_at"`
	LastError   string    `json:"last_error"`
	Failures    int       `json:"failures"`
	Successes   int       `json:"successes"`
	BoundCount  int       `json:"bound_count"`
	// Exit 出口画像（国家 / 住宅或机房 / ISP / ASN）。空字段 = 不知道，不是「没问题」。
	Exit ExitInfo `json:"exit"`
}

type Snapshot struct {
	Proxies  []View             `json:"proxies"`
	Bindings map[string]Binding `json:"bindings"`
	Realms   map[string]Binding `json:"realms"`
	Health   Settings           `json:"health"`
}

type Input struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Password string `json:"password"`
	Enabled  *bool  `json:"enabled"`
}

type ImportResult struct {
	Line   int    `json:"line"`
	Status string `json:"status"`
	ID     string `json:"id,omitempty"`
	Error  string `json:"error,omitempty"`
	// Name 订阅里给这个节点的名字（Clash 的 name / URI 的 #fragment）。仅作展示：
	// 面板据此告诉用户「这条被跳过的是哪个节点」，比只报行号有用得多。
	Name string `json:"name,omitempty"`
}

type Manager struct {
	mu         sync.Mutex
	path       string
	data       disk
	health     map[string]health
	generation map[string]uint64
	checking   map[string]bool
	requested  map[string]bool
	transports map[string]*http.Transport
	base       *http.Transport
	dialer     *net.Dialer
	tlsOver    TLSOverFunc // 经代理的 HTTPS 用它在隧道上握手（SetTLSOver）；nil = net/http 原生握手
	closed     bool
	wake       chan struct{}
	// Shared by periodic and manual probes, including concurrent HTTP handlers.
	slots chan struct{}
	// exit 每个节点的出口画像（ip / 国家 / 住宅或机房 / ISP），见 intel.go。
	// 持久化不必要：重启后第一次探测会重新取。
	exit map[string]ExitInfo
	// lookupExitFn 出口画像的查询函数；nil = 关闭（默认，见 EnableExitLookup）。
	lookupExitFn func(context.Context) ExitInfo
}

func New(path string, base *http.Transport, dialer *net.Dialer) (*Manager, error) {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport)
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	}
	m := &Manager{
		path: path, base: base.Clone(), dialer: dialer,
		data:   disk{Nodes: []node{}, Bindings: map[string]Binding{}, Realms: map[string]Binding{}, Settings: DefaultSettings()},
		health: map[string]health{}, generation: map[string]uint64{}, checking: map[string]bool{}, requested: map[string]bool{},
		transports: map[string]*http.Transport{}, wake: make(chan struct{}, 1), slots: make(chan struct{}, 8),
		exit: map[string]ExitInfo{},
	}
	if path != "" {
		raw, err := safefile.Read(path, nil)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := json.Unmarshal(raw, &m.data); err != nil {
				return nil, fmt.Errorf("parse proxy pool: %w", err)
			}
		}
	}
	if err := m.data.Settings.validate(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range m.data.Nodes {
		n := &m.data.Nodes[i]
		u, err := parseURL(n.URL)
		if err != nil || n.ID == "" || m.generation[n.ID] != 0 || seen[uString(u)] {
			return nil, errors.New("invalid or duplicate proxy in stored pool")
		}
		n.URL = u.String()
		seen[n.URL] = true
		m.generation[n.ID] = 1
	}
	if m.data.Bindings == nil {
		m.data.Bindings = map[string]Binding{}
	}
	for _, b := range m.data.Bindings {
		if err := m.validateBindingLocked(b); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func uString(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return nil, errors.New("invalid proxy URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	defaultPort := map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
	if defaultPort == "" || u.Hostname() == "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || (u.User != nil && u.User.Username() == "") {
		return nil, errors.New("proxy URL requires http/https/socks5/socks5h, host and optional credentials; paths and queries are unsupported")
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("proxy port must be 1-65535")
	}
	u.Host = net.JoinHostPort(strings.ToLower(u.Hostname()), strconv.Itoa(n))
	u.Path, u.RawPath = "", ""
	return u, nil
}

func (m *Manager) copyLocked() disk {
	d := m.data
	d.Nodes = append([]node{}, d.Nodes...)
	d.Bindings = make(map[string]Binding, len(m.data.Bindings))
	for k, v := range m.data.Bindings {
		d.Bindings[k] = v
	}
	return d
}

func (m *Manager) commitLocked(d disk) error {
	if m.closed {
		return errors.New("proxy pool is closed")
	}
	if m.path != "" {
		raw, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
			return err
		}
		if err := safefile.WriteKeep(m.path, raw, 0o600); err != nil {
			return err
		}
	}
	m.data = d
	m.signal()
	return nil
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	s := Snapshot{Proxies: []View{}, Bindings: d.Bindings, Realms: d.Realms, Health: d.Settings}
	counts := map[string]int{}
	for _, b := range d.Bindings {
		if b.Mode == "fixed" {
			counts[b.ProxyID]++
		}
	}
	for _, n := range d.Nodes {
		h := m.health[n.ID]
		status := h.status
		if status == "" {
			status = "pending"
		}
		if !n.Enabled {
			status = "disabled"
		}
		u, _ := parseURL(n.URL)
		passwordSet := false
		if u.User != nil {
			_, passwordSet = u.User.Password()
			u.User = url.User(u.User.Username())
		}
		s.Proxies = append(s.Proxies, View{
			ID: n.ID, Name: n.Name, URL: u.String(), Protocol: u.Scheme, PasswordSet: passwordSet,
			Enabled: n.Enabled, Status: status, Checking: m.checking[n.ID], LatencyMS: h.latencyMS,
			CheckedAt: h.checkedAt, LastError: h.lastError, Failures: h.failures, Successes: h.successes, BoundCount: counts[n.ID],
			Exit: m.exit[n.ID],
		})
	}
	return s
}

func (m *Manager) Import(text string) ([]ImportResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	seen := map[string]string{}
	for _, n := range d.Nodes {
		seen[n.URL] = n.ID
	}
	results := []ImportResult{}
	var added []string
	// 订阅归一化（subscription.go）：明文 URI / 整段 base64 / Clash YAML 三种形态
	// 都在这里变成「一行一个受支持的 URL」。认不出的条目带 Reason 回来，按
	// unsupported / invalid 逐条报账——**不静默丢弃**，否则用户粘了 200 个节点
	// 只进来 3 个却不知道为什么。
	entries, err := ParseSubscription(text)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		r := ImportResult{Line: entry.Line, Name: entry.Name}
		if entry.URL == "" {
			// 认得出来但协议不支持 → unsupported（面板不计入「无效」，也不把整段
			// 订阅原样回填到输入框：那对用户没有意义）。真认不出的 → invalid。
			r.Status = "invalid"
			if entry.Unsupported {
				r.Status = "unsupported"
			}
			r.Error = entry.Reason
			results = append(results, r)
			continue
		}
		r.Status = "invalid"
		u, err := parseURL(entry.URL)
		if err != nil {
			r.Error = err.Error()
		} else if id := seen[u.String()]; id != "" {
			r.Status, r.ID = "duplicate", id
		} else {
			var b [16]byte
			if _, err := cryptorand.Read(b[:]); err != nil {
				return nil, err
			}
			id := hex.EncodeToString(b[:])
			d.Nodes = append(d.Nodes, node{ID: id, Name: nodeName(u.Host, entry.Name), URL: u.String(), Enabled: true})
			seen[u.String()] = id
			added = append(added, id)
			r.Status, r.ID = "added", id
		}
		results = append(results, r)
	}
	if len(added) > 0 {
		if err := m.commitLocked(d); err != nil {
			return nil, err
		}
		for _, id := range added {
			m.generation[id] = 1
		}
	}
	return results, nil
}

// nodeName 节点显示名：订阅给了名字就用它（最多 128 字节，与 Update 的长度上限
// 一致），否则沿用 host。截断按字节做而不是按 rune——上限本来就是字节语义
// （Update 里 len(in.Name) > 128），两个入口必须同一把尺子。
func nodeName(host, want string) string {
	name := strings.TrimSpace(want)
	if name == "" {
		return host
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}

func (m *Manager) Update(id string, in Input) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	for i, n := range d.Nodes {
		if n.ID != id {
			continue
		}
		u, _ := parseURL(n.URL)
		if in.URL != "" {
			var err error
			u, err = parseURL(in.URL)
			if err != nil {
				return err
			}
			if u.User != nil {
				old, _ := parseURL(n.URL)
				_, supplied := u.User.Password()
				if !supplied && old.User != nil {
					if password, ok := old.User.Password(); ok {
						u.User = url.UserPassword(u.User.Username(), password)
					}
				}
			}
		}
		if in.Password != "" {
			if u.User == nil {
				return errors.New("password requires a username in the proxy URL")
			}
			u.User = url.UserPassword(u.User.Username(), in.Password)
		}
		for _, other := range d.Nodes {
			if other.ID != id && other.URL == u.String() {
				return errors.New("duplicate proxy URL")
			}
		}
		if in.Name != "" {
			if len(in.Name) > 128 {
				return errors.New("proxy name must not exceed 128 bytes")
			}
			d.Nodes[i].Name = strings.TrimSpace(in.Name)
		}
		d.Nodes[i].URL = u.String()
		if in.Enabled != nil {
			d.Nodes[i].Enabled = *in.Enabled
		}
		reset := d.Nodes[i].URL != n.URL || d.Nodes[i].Enabled != n.Enabled
		if err := m.commitLocked(d); err != nil {
			return err
		}
		if reset {
			m.resetLocked(id)
		}
		return nil
	}
	return ErrNotFound
}

func (m *Manager) resetLocked(id string) {
	m.generation[id]++
	delete(m.health, id)
	delete(m.exit, id) // 换了地址就是换了出口，旧画像不能留着
	if tr := m.transports[id]; tr != nil {
		tr.CloseIdleConnections()
		delete(m.transports, id)
	}
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.data.Bindings {
		if b.Mode == "fixed" && b.ProxyID == id {
			return ErrBound
		}
	}
	for _, b := range m.data.Realms {
		if b.Mode == "fixed" && b.ProxyID == id {
			return ErrBound
		}
	}
	d := m.copyLocked()
	for i, n := range d.Nodes {
		if n.ID == id {
			d.Nodes = append(d.Nodes[:i], d.Nodes[i+1:]...)
			if err := m.commitLocked(d); err != nil {
				return err
			}
			m.resetLocked(id)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Manager) validateBindingLocked(b Binding) error {
	switch b.Mode {
	case "random", "direct":
		if b.ProxyID != "" {
			return errors.New("proxy_id is only valid in fixed mode")
		}
	case "fixed":
		for _, n := range m.data.Nodes {
			if n.ID == b.ProxyID {
				return nil
			}
		}
		return ErrNotFound
	default:
		return errors.New("proxy mode must be random, direct or fixed")
	}
	return nil
}

func (m *Manager) Bind(uid string, b Binding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateBindingLocked(b); err != nil {
		return err
	}
	d := m.copyLocked()
	if b.Mode == "random" {
		delete(d.Bindings, uid)
	} else {
		d.Bindings[uid] = b
	}
	return m.commitLocked(d)
}

func (m *Manager) Unbind(uid string) error { return m.Bind(uid, Binding{Mode: "random"}) }

// BindRealm 设置 realm 级默认出口（issue #39 P3）。random = 恢复「随机池」，
// 等价于删掉这条默认；direct = 该域直连。账号级绑定优先于它。
func (m *Manager) BindRealm(realm string, b Binding) error {
	if realmKey(realm) == "" {
		return errors.New("realm must be cn or global")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateBindingLocked(b); err != nil {
		return err
	}
	d := m.copyLocked()
	if d.Realms == nil {
		d.Realms = map[string]Binding{}
	}
	if b.Mode == "random" {
		delete(d.Realms, realmKey(realm))
	} else {
		d.Realms[realmKey(realm)] = b
	}
	return m.commitLocked(d)
}

// RequestCheck queues probes on the service lifecycle, independent of browser disconnects.
// An empty ID requests all nodes, including manually disabled nodes.
func (m *Manager) RequestCheck(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	found := id == ""
	for _, n := range m.data.Nodes {
		if id == "" || n.ID == id {
			m.requested[n.ID] = true
			found = true
		}
	}
	if !found {
		return ErrNotFound
	}
	m.signal()
	return nil
}

func (m *Manager) Configure(s Settings) error {
	if err := s.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.copyLocked()
	if d.Settings == s {
		return nil
	}
	d.Settings = s
	if err := m.commitLocked(d); err != nil {
		return err
	}
	for _, n := range d.Nodes {
		m.resetLocked(n.ID)
	}
	return nil
}

func (m *Manager) selectLocked(uid, realm string) (node, bool, error) {
	if m.closed {
		return node{}, false, &Error{Reason: "pool closed"}
	}
	b := m.data.Bindings[uid]
	// 账号没有显式绑定时取 realm 默认出口（issue #39 P3）：查表顺序
	// 账号绑定 > realm 默认 > 随机池 > 直连。显式 direct 依旧赢过 realm 默认。
	if _, bound := m.data.Bindings[uid]; !bound {
		if rb, ok := m.data.Realms[realmKey(realm)]; ok {
			b = rb
		}
	}
	// 随机绑定下「一条启用中的代理都没有」= 直连：原来只看节点条数，代理全部被手工
	// 停用时仍进抽签、count==0，于是每个请求都 503「no healthy proxy available」
	// （issue #150）。停用即关闭，与「一条代理都没配」同义。
	// 只对非 fixed 生效：显式钉住的代理不可用时静默直连，等于把本机 IP 暴露给上游。
	if b.Mode == "direct" {
		return node{}, false, nil
	}
	if b.Mode != "fixed" {
		anyEnabled := false
		for _, n := range m.data.Nodes {
			if n.Enabled {
				anyEnabled = true
				break
			}
		}
		if !anyEnabled {
			return node{}, false, nil
		}
	}
	var selected node
	count := 0
	for _, n := range m.data.Nodes {
		if b.Mode == "fixed" && b.ProxyID != n.ID {
			continue
		}
		if n.Enabled && m.health[n.ID].status == "healthy" {
			count++
			if rand.IntN(count) == 0 {
				selected = n
			}
		}
	}
	if count == 0 {
		return node{}, false, &Error{ProxyID: b.ProxyID, Reason: "no healthy proxy available"}
	}
	return selected, true, nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, tr := range m.transports {
		tr.CloseIdleConnections()
	}
}
