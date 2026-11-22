package proxypool

import (
	"math/rand/v2"
	"net/http"
	"strings"
)

// 本文件是「登录/注册这类还没有账号的出站」的出口选择。
//
// 为什么需要单独一条路：selectLocked 的三档查表（账号绑定 > realm 默认 > 随机池）
// 全部以 uid 为键，而登录那一刻 uid 还不存在——于是三档一起落空，最后落到直连，
// 把机房 IP 交给上游。更麻烦的是「加号用 A 出口、之后业务用 B 出口」：同一批请求
// 前后换了 IP，本身就是风控眼里的强关联信号，而且事后无法回答「这号是哪条出口
// 注册的」。所以加号时要先钉一条出口，落盘后再把它写成该号的固定绑定
// （见 panel 侧 bindLoginProxy）。

// PickForLogin 从启用节点里随机挑一条给出站用，返回节点 ID；空串 = 池里一条启用
// 节点都没有（调用方按既有行为直连——没配代理的部署不该因此加不了号）。
//
// 优先「启用且健康」：健康是探测结论，直接用最省事。一条健康的都没有时**仍从启用
// 节点里挑**，而不是回落直连：宁可这次登录报代理错误让用户重试，也不静默换成机房
// IP——加号恰恰是最不该露本机地址的一步。健康状态只在内存里（重启清零），所以
// 「刚重启、还没探测完」也走这个分支，否则重启后的第一个号就直连了。
func (m *Manager) PickForLogin() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ""
	}
	healthy := make([]string, 0, len(m.data.Nodes))
	enabled := make([]string, 0, len(m.data.Nodes))
	for _, n := range m.data.Nodes {
		if !n.Enabled {
			continue
		}
		enabled = append(enabled, n.ID)
		if m.health[n.ID].status == "healthy" {
			healthy = append(healthy, n.ID)
		}
	}
	if len(healthy) > 0 {
		return healthy[rand.IntN(len(healthy))]
	}
	if len(enabled) > 0 {
		return enabled[rand.IntN(len(enabled))]
	}
	return ""
}

// NodeLabel 返回节点的可读标签（名称优先，退化为 ID 前缀），给日志与回显用。
//
// 只用名称与 ID：URL 里可能带账号密码，任何对外文本（日志、面板响应）都不带它，
// 与 Snapshot 的脱敏口径一致。节点已删除时返回 ID 前缀（历史绑定仍能对上）。
func (m *Manager) NodeLabel(id string) string {
	if id == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.data.Nodes {
		if n.ID != id {
			continue
		}
		if name := strings.TrimSpace(n.Name); name != "" {
			return name
		}
		break
	}
	return shortID(id)
}

// shortID 日志用的 ID 前缀。
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// DoPinned 用指定节点发送一次请求（登录/注册这类还没有账号的出站）。
//
// 与 Do 的差别只在「怎么选」：Do 查绑定表，这里直接钉 id。选定之后完全复用
// doVia，错误归类与连接清理不会分叉。
//
// 节点不存在 / 池已关闭一律返回 *Error，**不静默直连**：出口是加号与后续任务
// 共同的事实，悄悄改道等于把本机 IP 暴露出去，还让「这号绑了哪条出口」变成谎话。
// 调用方要直连只有两条明路：空 id，或 pickLoginRoute 一开始就没选（池里无节点）。
func (m *Manager) DoPinned(client *http.Client, req *http.Request, id string) (*http.Response, error) {
	if id == "" {
		return client.Do(req)
	}
	m.mu.Lock()
	var (
		n   node
		tr  *http.Transport
		err error
	)
	switch {
	case m.closed:
		err = &Error{ProxyID: id, Reason: "pool closed"}
	default:
		found := false
		for _, cand := range m.data.Nodes {
			if cand.ID == id {
				n, found = cand, true
				break
			}
		}
		if !found {
			err = &Error{ProxyID: id, Reason: "proxy not found", cause: ErrNotFound}
		} else if tr, err = m.transportLocked(n); err != nil {
			err = &Error{ProxyID: id, Reason: "proxy transport unavailable", cause: err}
		}
	}
	m.mu.Unlock()
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return doVia(client, req, n, tr)
}
