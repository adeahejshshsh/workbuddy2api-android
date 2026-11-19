// Package proxypool manages persistent account routes and independently probed proxies.
package proxypool

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// Format 让 %v / %+v / %s 一律只输出脱敏后的 Reason（审查 W2-C6-18）。
//
// 为什么需要：Error() 已经不含 URL，但上层若用 %+v 或直接打印 Unwrap 出来的
// cause（典型是 *url.Error，其 Error() 里带完整 URL —— 代理凭据就在
// user:pass@host 段），凭据就进了日志。Unwrap 本身必须保留（errors.Is/As 要用），
// 所以这里用 fmt.Formatter 把**所有格式动词**都收口到脱敏文本：
//
//	fmt.Printf("%v", err)  /  "%+v"  /  "%s"  →  "proxy route: <reason>"
//
// 需要底层错误时走 errors.Unwrap(err) 显式取，那是「我知道自己在做什么」的写法。
func (e *Error) Format(st fmt.State, verb rune) {
	_, _ = io.WriteString(st, e.Error())
}

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
	// AllowInternal 允许探测 URL 指向回环/内网（默认 **false**）。
	// 只给「探测器本身就在内网」的部署或测试用：默认拒绝是因为探测由服务端自己发起，
	// 放开等于给网关加一个可配置的 SSRF 入口（审查 W2-C6-14）。
	AllowInternal bool `json:"allow_internal,omitempty"`
}

func DefaultSettings() Settings {
	return Settings{URL: "https://www.workbuddy.cn/", IntervalSeconds: 60, TimeoutSeconds: 10,
		FailureThreshold: 3, SuccessThreshold: 2}
}

func (s Settings) validate() error {
	u, err := url.Parse(s.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return errors.New("health URL must be an HTTP(S) URL without credentials or fragment")
	}
	// 探测 URL 由服务端自己发起（审查 W2-C6-14）：此前只校验 scheme/host/无凭据，
	// 于是有配置权限者（或被劫持的订阅导入）可以让网关去打 169.254.169.254、
	// 127.0.0.1:6379、内网管理口，并把状态/延迟/错误回显到面板。
	if !s.AllowInternal {
		if err := rejectInternalHost(u.Hostname()); err != nil {
			return fmt.Errorf("health URL %w（确需探测内网请显式设 allow_internal=true）", err)
		}
	}
	if s.IntervalSeconds < 1 || s.IntervalSeconds > 86400 || s.TimeoutSeconds < 1 || s.TimeoutSeconds > 300 ||
		s.FailureThreshold < 1 || s.FailureThreshold > 100 || s.SuccessThreshold < 1 || s.SuccessThreshold > 100 {
		return errors.New("health interval must be 1-86400s, timeout 1-300s, thresholds 1-100")
	}
	return nil
}

// rejectInternalHost 拒绝指向本机/内网/链路本地/云元数据的探测目标。
//
// 只校验**字面量**地址：不做 DNS（解析发生在拨号时，先解析再拨号仍有 TOCTOU，且会把
// 解析器变成新的失败点）。IP 字面量与已知元数据域名已经覆盖绝大多数误配与滥用。
func rejectInternalHost(host string) error {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "" {
		return errors.New("主机为空")
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		h == "metadata.google.internal" || h == "metadata" ||
		strings.HasSuffix(h, ".internal") {
		return fmt.Errorf("不能指向内部主机 %q", host)
	}
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("不能指向内网/回环地址 %q", host)
		}
		// 云元数据常见别名段（169.254 已被 LinkLocal 覆盖，这里补 CGNAT 与保留段）。
		if ip4 := ip.To4(); ip4 != nil {
			if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 { // 100.64.0.0/10 CGNAT
				return fmt.Errorf("不能指向运营商级 NAT 地址 %q", host)
			}
			if ip4[0] == 0 {
				return fmt.Errorf("不能指向保留地址 %q", host)
			}
		}
	}
	return nil
}

// maxImportNodes / maxImportBytes 单次订阅导入的硬上限（审查 W2-C6-13）：
// 此前解析与整表写盘全程持锁，一次几十 MB / 10 万行的粘贴就能让 selectLocked
// （每请求热路径）排队到超时。
const (
	maxImportNodes = 5000
	maxImportBytes = 4 << 20
)

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
	mu sync.Mutex
	// writeMu IO 顺序锁（W4-09）：串行化「提交序」，保证落盘顺序与状态变更顺序
	// 一致（旧提交不会后落盘覆盖新提交）。锁序固定 writeMu → mu，反向无路径。
	writeMu sync.Mutex
	// commitHook 仅供测试注入的同步点（W4-09 变异验证）：在「m.mu 已放、落盘尚未
	// 开始」这一刻回调，用来证明 fsync 不在状态锁内。生产路径恒 nil。
	commitHook atomic.Pointer[func()]
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
	// exitLookupOn 是否在健康探测时查出口画像；缺省关闭（见 SetExitLookup）。
	exitLookupOn bool
	// directFallbacks 「随机/realm 模式下没有任何启用节点 → 静默直连」的次数
	// （审查 W2-C6-17）。直连语义保持不变（issue #150 的既有约定：停用即关闭，
	// 与「一条代理都没配」同义），但必须**可观测**：运维以为流量走代理、实际
	// 直连时，本机 IP 就直接暴露给上游，与多账号隔离目标冲突。
	// 面板/监控可据此计数告警；日志侧按 directWarnEvery 节流提示。
	directFallbacks atomic.Int64
	lastDirectWarn  atomic.Int64 // 上次告警的 unix 秒（节流）
}

// directWarnEvery 直连回落告警的最小间隔（审查 W2-C6-17）。
// 10 分钟：既不会在每请求直连时刷屏，也不至于让「配错代理」长期无声。
const directWarnEvery = 600

// DirectFallbacks 返回「无启用节点 → 直连」的累计次数（审查 W2-C6-17）。
// 调用方（面板/监控）据此判断「代理池是否实际没在用」。
func (m *Manager) DirectFallbacks() int64 { return m.directFallbacks.Load() }

// noteDirectFallback 记一次直连回落并节流告警（审查 W2-C6-17）。调用方持 m.mu。
func (m *Manager) noteDirectFallback(uid, realm string) {
	m.directFallbacks.Add(1)
	now := time.Now().Unix()
	last := m.lastDirectWarn.Load()
	if now-last < directWarnEvery {
		return
	}
	if !m.lastDirectWarn.CompareAndSwap(last, now) {
		return // 另一个请求刚打过
	}
	log.Printf("WARN: [proxypool] 无启用中的代理节点，本次请求直连（本机 IP 将暴露给上游）uid=%s realm=%s 累计=%d",
		uid, realmKey(realm), m.directFallbacks.Load())
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
	host := strings.ToLower(u.Hostname())
	if !validProxyHost(host) {
		return nil, errors.New("proxy host must be an IP literal or [a-z0-9._-] hostname")
	}
	u.Host = net.JoinHostPort(host, strconv.Itoa(n))
	u.Path, u.RawPath = "", ""
	return u, nil
}

// validProxyHost 代理主机段策略（审查 W2-C6-19）。
//
// 此前只校验「非空」：host 里出现 @ / % / 反斜杠 / 空白 / 非 ASCII（IDN）时照样入库，
// 而 host 会被拼进拨号地址与 CONNECT 请求行 —— 这是「面板展示的 host 与实际拨号目标
// 可以不一致」的土壤（钓鱼式误导运维），也是 IDN 同形字攻击面。
// 只接受两类：IP 字面量（v4/v6，net.ParseIP 判定），或纯 ASCII 的主机名
// （字母数字 + . - _）。IDN 请先用 punycode 写好再导入。
func validProxyHost(h string) bool {
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

func (m *Manager) copyLocked() disk {
	d := m.data
	d.Nodes = append([]node{}, d.Nodes...)
	d.Bindings = make(map[string]Binding, len(m.data.Bindings))
	for k, v := range m.data.Bindings {
		d.Bindings[k] = v
	}
	// Realms 也必须深拷（审查 C1）：此前只拷 Nodes/Bindings，Realms 仍是**同一个 map**。
	// BindRealm 在写锁内原地改它，而 Snapshot() 把这个 map 直接交给调用方（锁外遍历）——
	// 一次并发绑定就是 concurrent map read and map write，Go 运行时直接 fatal（不可 recover，
	// 整进程退出、所有在途 SSE 一起断）。
	d.Realms = make(map[string]Binding, len(m.data.Realms))
	for k, v := range m.data.Realms {
		d.Realms[k] = v
	}
	return d
}

// mutateCommit 执行一次「改状态 + 落盘」提交（W4-09）。
//
// 为什么需要这个形状：commitLocked 原先在 m.mu 内做 json.MarshalIndent +
// MkdirAll + WriteKeep（内含 fsync/rename）。磁盘慢或 bind mount 时单次几十毫秒，
// 而这把锁是 selectLocked（每请求热路径）、Snapshot（面板轮询）共用的 —— 表现为
// 整网关偶发卡顿。拆成「锁内改状态、锁外 IO」又不能无序：两个并发提交里旧的那份
// 可能后落盘，把配置写回上一版（与 pool 的 C-02 完全同形）。
//
// 所以用一把独立的 writeMu 做 IO 顺序锁，锁序固定 **writeMu → m.mu**：
//  1. 先拿 writeMu：从这一刻起，本次提交在「提交序」里占位；
//  2. 拿 m.mu，执行 fn 构造新状态并序列化，然后**立刻放掉 m.mu**；
//  3. 在 writeMu 保护下落盘（fsync 不再阻塞读路径）；
//  4. 落盘成功后才发布 m.data —— 与旧语义一致：写失败时运行态不变
//     （TestWriteFailureDoesNotPublish 钉的就是这条）。
//
// fn 在 m.mu 内执行，可以安全调用 copyLocked/resetLocked 等 *Locked 方法；
// changed=false 表示「无需写盘」（例如配置没变化）。
func (m *Manager) mutateCommit(fn func() (disk, bool, error)) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("proxy pool is closed")
	}
	d, changed, err := fn()
	var raw []byte
	if err == nil && changed && m.path != "" {
		raw, err = json.MarshalIndent(d, "", "  ")
	}
	m.mu.Unlock()
	if err != nil || !changed {
		return err
	}
	if hook := m.commitHook.Load(); hook != nil {
		(*hook)()
	}
	if m.path != "" {
		if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
			return err
		}
		if err := safefile.WriteKeep(m.path, raw, 0o600); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.data = d
	m.signal()
	m.mu.Unlock()
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
		// parseURL 出错返回 nil（审查 W2-C6-12）：此前丢弃 error 后立刻解引用 u.User，
		// 配置文件被外部编辑、或将来新增未校验写入路径时，面板任意接口直接 panic
		// （HTTP handler 崩，且 recover 后会丢掉整个响应）。
		u, uerr := parseURL(n.URL)
		if uerr != nil || u == nil {
			s.Proxies = append(s.Proxies, View{
				ID: n.ID, Name: n.Name, URL: n.URL, Protocol: "", PasswordSet: false,
				Enabled: n.Enabled, Status: status, Checking: m.checking[n.ID], LatencyMS: h.latencyMS,
				CheckedAt: h.checkedAt, LastError: "URL 无法解析: " + errText(uerr), Failures: h.failures,
				Successes: h.successes, BoundCount: counts[n.ID], Exit: m.exit[n.ID],
			})
			continue
		}
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
	// 解析放到锁外（审查 W2-C6-13）：此前 ParseSubscription 与整表写盘全程持 m.mu，
	// 而 selectLocked 是每请求热路径 —— 一次几十 MB / 10 万行的粘贴就能让全网关请求
	// 排队到超时。这里先按字节数硬拒，再在锁外解析，锁内只做「查重 + 追加」。
	if len(text) > maxImportBytes {
		return nil, fmt.Errorf("订阅内容 %d 字节超过上限 %d（请分批导入）", len(text), maxImportBytes)
	}
	// 订阅归一化（subscription.go）：明文 URI / 整段 base64 / Clash YAML 三种形态
	// 都在这里变成「一行一个受支持的 URL」。认不出的条目带 Reason 回来，按
	// unsupported / invalid 逐条报账——**不静默丢弃**，否则用户粘了 200 个节点
	// 只进来 3 个却不知道为什么。
	entries, err := ParseSubscription(text)
	if err != nil {
		return nil, err
	}
	if len(entries) > maxImportNodes {
		return nil, fmt.Errorf("订阅含 %d 条，超过单次上限 %d（请分批导入）", len(entries), maxImportNodes)
	}

	// W4-09：改状态与落盘拆开（mutateCommit 内锁外 fsync），提交序由 writeMu 保证。
	var results []ImportResult
	var added []string
	err = m.mutateCommit(func() (disk, bool, error) {
		d := m.copyLocked()
		seen := map[string]string{}
		for _, n := range d.Nodes {
			seen[n.URL] = n.ID
		}
		results = []ImportResult{}
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
					return disk{}, false, err
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
			for _, id := range added {
				m.generation[id] = 1
			}
		}
		return d, len(added) > 0, nil
	})
	if err != nil {
		return nil, err
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
	// W4-09：改状态在 m.mu 内，fsync 在 writeMu 保护下、m.mu 之外。
	return m.mutateCommit(func() (disk, bool, error) {
		d := m.copyLocked()
		for i, n := range d.Nodes {
			if n.ID != id {
				continue
			}
			// 存量 URL 解析失败时不能带着 nil 往下走（审查 W2-C6-12 同类）：
			// 下面 u.User 会直接解引用 —— 配置被外部改坏就是一次面板接口 panic。
			u, perr := parseURL(n.URL)
			if perr != nil {
				u = nil
			}
			if in.URL != "" {
				var err error
				u, err = parseURL(in.URL)
				if err != nil {
					return disk{}, false, err
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
				if u == nil {
					return disk{}, false, errors.New("invalid stored proxy URL")
				}
				if u.User == nil {
					return disk{}, false, errors.New("password requires a username in the proxy URL")
				}
				u.User = url.UserPassword(u.User.Username(), in.Password)
			}
			for _, other := range d.Nodes {
				if other.ID != id && other.URL == u.String() {
					return disk{}, false, errors.New("duplicate proxy URL")
				}
			}
			if in.Name != "" {
				if len(in.Name) > 128 {
					return disk{}, false, errors.New("proxy name must not exceed 128 bytes")
				}
				d.Nodes[i].Name = strings.TrimSpace(in.Name)
			}
			d.Nodes[i].URL = u.String()
			if in.Enabled != nil {
				d.Nodes[i].Enabled = *in.Enabled
			}
			if d.Nodes[i].URL != n.URL || d.Nodes[i].Enabled != n.Enabled {
				// 换地址/启停 = 换出口：旧连接池与健康台账都作废。这是纯内存副作用，
				// 落盘失败时最多多重连一次（比把 fsync 塞回锁内便宜得多）。
				m.resetLocked(id)
			}
			return d, true, nil
		}
		return disk{}, false, ErrNotFound
	})
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
	removed := false
	err := m.mutateCommit(func() (disk, bool, error) {
		for _, b := range m.data.Bindings {
			if b.Mode == "fixed" && b.ProxyID == id {
				return disk{}, false, ErrBound
			}
		}
		for _, b := range m.data.Realms {
			if b.Mode == "fixed" && b.ProxyID == id {
				return disk{}, false, ErrBound
			}
		}
		d := m.copyLocked()
		for i, n := range d.Nodes {
			if n.ID == id {
				d.Nodes = append(d.Nodes[:i], d.Nodes[i+1:]...)
				removed = true
				return d, true, nil
			}
		}
		return disk{}, false, ErrNotFound
	})
	if err != nil || !removed {
		return err
	}
	// 落盘成功后再清运行态（写失败时节点还在，generation/health 必须一起留着）。
	m.mu.Lock()
	m.resetLocked(id)
	// 删节点时把两张只增不删的表一起清掉（审查 W2-C6-16）：订阅刷新场景下反复
	// 导入/删除，generation 与 requested 会单调增长，长期运行就是内存泄漏。
	delete(m.generation, id)
	delete(m.requested, id)
	delete(m.checking, id)
	m.mu.Unlock()
	return nil
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
	return m.mutateCommit(func() (disk, bool, error) {
		if err := m.validateBindingLocked(b); err != nil {
			return disk{}, false, err
		}
		d := m.copyLocked()
		if b.Mode == "random" {
			delete(d.Bindings, uid)
		} else {
			d.Bindings[uid] = b
		}
		return d, true, nil
	})
}

func (m *Manager) Unbind(uid string) error { return m.Bind(uid, Binding{Mode: "random"}) }

// BindRealm 设置 realm 级默认出口（issue #39 P3）。random = 恢复「随机池」，
// 等价于删掉这条默认；direct = 该域直连。账号级绑定优先于它。
func (m *Manager) BindRealm(realm string, b Binding) error {
	if realmKey(realm) == "" {
		return errors.New("realm must be cn or global")
	}
	return m.mutateCommit(func() (disk, bool, error) {
		if err := m.validateBindingLocked(b); err != nil {
			return disk{}, false, err
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
		return d, true, nil
	})
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
	var nodes []node
	err := m.mutateCommit(func() (disk, bool, error) {
		d := m.copyLocked()
		if d.Settings == s {
			return d, false, nil
		}
		d.Settings = s
		nodes = d.Nodes
		return d, true, nil
	})
	if err != nil {
		return err
	}
	// 探测参数变化 → 全部节点的健康台账/连接池作废（内存副作用，见 Update 同款说明）。
	if len(nodes) > 0 {
		m.mu.Lock()
		for _, n := range nodes {
			m.resetLocked(n.ID)
		}
		m.mu.Unlock()
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
			// 直连语义保留（issue #150），但计数 + 节流告警（审查 W2-C6-17）。
			m.noteDirectFallback(uid, realm)
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

// errText 把 error 压成一段可放进 View.LastError 的短文本（nil 安全）。
func errText(err error) string {
	if err == nil {
		return "未知错误"
	}
	return err.Error()
}
