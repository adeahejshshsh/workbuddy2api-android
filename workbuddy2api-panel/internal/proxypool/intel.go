// intel.go 出口归属情报（借鉴 ardeyouxipianyi/workbuddy2api-hub 的 wb_ipintel.py）。
//
// 代理池原来只记「走哪个代理」，不记「这个出口是什么网络」。排查 11140 / 封禁类问题时
// 这两件事要分开：是某个号的行为触发，还是同一个机房的多个出口一起被降权——后者只能
// 从出口画像看出来（多个不同账号、不同出口、同一个 ASN/机房）。
//
// 一个请求就够：ip-api 的 hosting 标志即「住宅 / 机房」之分。它是**启发式**（宣布这段
// 地址的网络被标为机房），不是注册事实，所以 ISP/ASN 一并存下来给运维自己判。查询失败
// 记 unknown（空值），绝不猜——一个查不到归属的代理照样能跑流量。
//
// 请求**直连**发出而不经被描述的那个代理：查询要在「该代理只通上游」时仍然可用，也不能
// 由被询问的那个出口来回答自己。
package proxypool

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// intelURL ip-api 一次查询同时给出退出 IP 与其归属（fields 只取要用的，省流量）。
// var 而非 const：包内测试指向本地 httptest 服务，不碰外网。
var intelURL = "http://ip-api.com/json/?lang=zh-CN" +
	"&fields=status,message,query,country,countryCode,isp,org,as,hosting"

// intelTTL 出口画像的缓存时长：出口 IP 不会天天换，但换机房/换节点是常事；太短会把
// 每次健康探测都变成两次请求，太长则换出口后一直显示旧画像。
const intelTTL = 6 * time.Hour

// ExitInfo 一个出口的画像。每个字段都可能为空，空 = 不知道，不等于「没问题」。
type ExitInfo struct {
	IP          string `json:"ip,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	// Kind 地址类型：residential / datacenter；空 = 上游没给 hosting 标志。
	Kind string    `json:"ip_type,omitempty"`
	ISP  string    `json:"isp,omitempty"`
	ASN  string    `json:"asn,omitempty"`
	At   time.Time `json:"at,omitempty"`
}

// fresh 报告这份画像是否还在缓存期内。
func (e ExitInfo) fresh(now time.Time) bool {
	return !e.At.IsZero() && now.Sub(e.At) < intelTTL
}

// classifyExit 把一次 ip-api 回包转成出口画像。
//
// 任何不可用的形态（status 不是 success、hosting 既不是 true 也不是 false）都回空值，
// 不按「大概率是住宅」之类猜。
func classifyExit(raw []byte, now time.Time) ExitInfo {
	var p struct {
		Status      string `json:"status"`
		Query       string `json:"query"`
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		ISP         string `json:"isp"`
		Org         string `json:"org"`
		AS          string `json:"as"`
		Hosting     *bool  `json:"hosting"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Status != "success" {
		return ExitInfo{}
	}
	kind := ""
	if p.Hosting != nil {
		if *p.Hosting {
			kind = "datacenter"
		} else {
			kind = "residential"
		}
	}
	isp := strings.TrimSpace(p.ISP)
	if isp == "" {
		isp = strings.TrimSpace(p.Org)
	}
	return ExitInfo{
		IP:          strings.TrimSpace(p.Query),
		Country:     strings.TrimSpace(p.Country),
		CountryCode: strings.ToUpper(strings.TrimSpace(p.CountryCode)),
		Kind:        kind,
		ISP:         isp,
		ASN:         strings.TrimSpace(p.AS),
		At:          now,
	}
}

// fetchExitInfo 直连查一次出口画像。
//
// 查询失败也写 At（空字段 + 时间戳）：否则每次健康探测都会重查一次，网络不通的部署
// 上就是每个号每个周期一次 DNS 超时。代价是一次抖动会让画像空 6 小时——画像空只影响
// 观测，不影响选号。
//
// 用 Manager 自己的 base transport（与「直连」账号同一条链路）：整套部署要走本机代理
// 才能出网时，新起一个默认 client 只会一直查不到，画像永远是 unknown。
func (m *Manager) fetchExitInfo(ctx context.Context) ExitInfo {
	now := time.Now()
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, intelURL, nil)
	if err != nil {
		return ExitInfo{At: now}
	}
	client := &http.Client{Transport: m.base}
	resp, err := client.Do(req)
	if err != nil {
		return ExitInfo{At: now}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ExitInfo{At: now}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return ExitInfo{At: now}
	}
	info := classifyExit(body, now)
	if info.At.IsZero() {
		info.At = now // 回包说 status != success：同样是「查过了，不知道」
	}
	return info
}

// EnableExitLookup 打开「健康探测时顺手查出口画像」。
//
// 默认关闭：它是一次真实的出网请求，单元测试里的代理节点都不该因此打到外网。生产装配
// 在起代理池之后调一次即可（cmd/server）。
func (m *Manager) EnableExitLookup() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookupExitFn = m.fetchExitInfo
}

// exitLookup 读当前查询函数（EnableExitLookup 会改它，故要持锁）。
func (m *Manager) exitLookup() func(context.Context) ExitInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookupExitFn
}

// exitStale 报告该节点的画像是否需要重查（不含已禁用/从未探测的节点语义，只问时间）。
func (m *Manager) exitStale(id string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.exit[id].fresh(now)
}

// setExitLocked 写入新画像（调用方持 m.mu）；出口 IP 变化时打一行日志——换出口是
// 排查时的关键时间点（同一号昨天正常、今天被封，往往就是出口换了）。
func (m *Manager) setExitLocked(id string, info ExitInfo) {
	if info.At.IsZero() {
		return // 查不到：保留旧画像，不把「不知道」写成空
	}
	prev := m.exit[id]
	m.exit[id] = info
	if prev.IP != info.IP {
		kind := map[string]string{"residential": "住宅", "datacenter": "机房"}[info.Kind]
		parts := []string{info.IP}
		if info.Country != "" {
			parts = append(parts, info.Country)
		}
		if kind != "" {
			parts = append(parts, kind)
		}
		if info.ISP != "" {
			parts = append(parts, info.ISP)
		}
		log.Printf("[proxypool] 出口变更 proxy=%s %s（同机房的多个出口一起异常时先看这里）",
			id, strings.Join(parts, " · "))
	}
}
