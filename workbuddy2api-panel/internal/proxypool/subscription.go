// subscription.go 代理订阅归一化：把「订阅内容」变成可导入的代理 URL（借鉴
// ardeyouxipianyi/workbuddy2api-hub 的 wb_subbridge.py 的输入侧）。
//
// 为什么需要：本网关的传输层只支持 http / https / socks5 三种代理（见 transport.go），
// 而订阅给的是 base64 的 ss/vmess/trojan 列表或 Clash YAML。此前唯一的导入入口是
// 「一行一个 http:// 或 socks5:// URL」，于是订阅拿到手还得手工转一遍。
//
// 本文件做的是**纯文本解析**，不做的事有三件，都是刻意的：
//
//  1. **不抓取订阅 URL**。面板进程去 GET 任意外部地址是 SSRF 面（内网地址探测、
//     云元数据端点），要有独立的域名白名单与出站审计才安全，不在这版范围。用户把
//     订阅内容（或订阅返回的正文）粘进来即可。
//  2. **不起 mihomo/clash 子进程**。参照实现为了支持 ss/vmess/trojan 起一个本地
//     mihomo 监听并渲染 yaml，那是把「网关」变成「代理编排器」：多一个二进制、
//     多一份配置生成器、多一组进程生命周期问题。本网关的取舍是不支持这些协议，
//     而是**明确报出被跳过的是哪些、为什么**（ImportResult.Status = "unsupported"
//     带上协议名），让运维知道该另外起一个本地 mihomo 再把它的 socks5 端口导入。
//  3. **不猜**。认不出的行报 invalid 并原样留在输入框里（面板既有行为），不做
//     「尽力而为」的模糊匹配——一条猜错的代理会把某个账号的流量发到不该去的地方。
package proxypool

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// SubEntry 订阅里的一项：要么归一出了受支持的 URL，要么带一条说明为什么不行。
type SubEntry struct {
	// Line 原文行号（1 起）。Clash 块式条目取该条目首行，便于用户在 yaml 里定位。
	Line int
	// Name 订阅里给的名字（Clash 的 name 字段 / URI 的 fragment），仅作展示。
	Name string
	// Kind 原始协议名（http / socks5 / ss / vmess / …），报错文案用。
	Kind string
	// URL 归一后的代理 URL（http/https/socks5）。空 = 不支持或无法解析。
	URL string
	// Unsupported true = 协议**认得出来**，只是本网关的传输层不解析它
	// （ss / vmess / trojan …）。与「认不出的行」是两件事：前者不是用户写错了，
	// 报「格式错误」会让人去改一个本来正确的东西。
	Unsupported bool
	// Reason 不支持 / 无法解析的原因（人类可读）。URL 非空时为空。
	Reason string
}

// supportedProxySchemes 传输层真正支持的协议（与 parseURL 的 defaultPort 同一份口径）。
var supportedProxySchemes = map[string]string{
	"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080",
}

// knownUnsupportedSchemes 认得出来、但本网关不解析的订阅协议。
//
// 列出来是为了给出**具体**的报错（「ss 不解析」）而不是笼统的「格式错误」——
// 后者会让用户以为是自己粘错了。
var knownUnsupportedSchemes = map[string]bool{
	"ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true,
	"hysteria": true, "hysteria2": true, "hy2": true, "tuic": true,
	"wireguard": true, "wg": true, "snell": true, "juicity": true, "anytls": true,
	"mieru": true, "ssh": true, "socks4": true, "socks4a": true,
}

// ParseSubscription 把订阅正文归一成条目列表。空输入返回空列表（不是错误）。
//
// 支持三种形态，自动识别：
//   - 明文 URI 列表（一行一个 http:// / socks5:// / ss:// …）；
//   - base64 正文（V2Ray/机场最常见的 ss/vmess 订阅，整段 base64）；
//   - Clash / mihomo 的 YAML 配置（只取 proxies: 段）。
func ParseSubscription(text string) ([]SubEntry, error) {
	// 刻意**不** TrimSpace 原文：行号是面板回填输入框的索引（无效行会被原样放回
	// 文本框让用户改），前面少几行就会错位——实测「首行是空行」的输入因此把第 2 行
	// 报成第 1 行。只在判空时 Trim。
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	if decoded, ok := decodeBase64Body(text); ok {
		text = strings.TrimSpace(decoded)
	}
	if hasClashProxies(text) {
		return parseClashProxies(text)
	}
	return parseURILines(text)
}

// decodeBase64Body 整段 base64 订阅（去掉换行后解码）。判据刻意收紧：
//
//	原文必须既不含 "://" 也不含 "proxies:" —— 那两种是明文订阅与 yaml 的特征，
//	先排除掉才不会把「恰好全由 base64 字符组成的配置」误当编码内容；
//	解码结果必须含 "://" 或 "proxies:" —— 否则这就是一段碰巧能解出来的字，不是订阅。
//
// 返回 ok=false 时调用方按原文继续处理（绝不因为「不像 base64」就丢弃输入）。
func decodeBase64Body(text string) (string, bool) {
	if strings.Contains(text, "://") || strings.Contains(strings.ToLower(text), "proxies:") {
		return "", false
	}
	compact := strings.Join(strings.Fields(text), "")
	if compact == "" {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		raw, err := enc.DecodeString(compact)
		if err != nil {
			continue
		}
		decoded := string(raw)
		if strings.Contains(decoded, "://") || strings.Contains(strings.ToLower(decoded), "proxies:") {
			return decoded, true
		}
	}
	return "", false
}

// hasClashProxies 报告正文是否是一个带 proxies: 段的 Clash 配置。
func hasClashProxies(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if isClashProxiesKey(line) {
			return true
		}
	}
	return false
}

// isClashProxiesKey 顶层（或任意缩进）的 `proxies:` 键。
//
// 允许缩进是因为有的订阅把 proxies 放在别的结构里；`proxies: []` 这种空值的也认
// （下面解析会得到 0 条，用户看到「没有可用节点」而不是「格式错误」）。
// 值为**流式序列**（`proxies: [{...}, {...}]`）时同样是合法的 proxies 段——此前只认
// 字面量 "[]"，于是这种写法整体落到 URI 行解析，产出一个既无 type 也无 server 的
// 空条目：节点被静默丢掉，还不报错。
func isClashProxiesKey(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "proxies:") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(t, "proxies:"))
	if rest == "" || strings.HasPrefix(rest, "#") {
		return true
	}
	return strings.HasPrefix(rest, "[")
}

// parseClashProxies 只解析 proxies: 段，其余段一概不看。
//
// 为什么手写而不引 YAML 库：仓库当前零 yaml 依赖，而为「读一个 {type, server, port,
// username, password}」这五个标量字段引一个通用解析器不值得——它带来的是一整套
// 类型系统与宽松语法的边界问题，而我们只需要扁平的 key: value。
//
// 支持两种写法（都是真实订阅里出现的）：
//
//   - name: 香港 01
//     type: socks5
//     server: hk.example.com
//     port: 1080
//     username: u
//     password: p
//
//   - {name: "香港 01", type: socks5, server: hk.example.com, port: 1080}
//
// 段内出现新的顶层键（proxy-groups: 等）即结束。
func parseClashProxies(text string) ([]SubEntry, error) {
	lines := strings.Split(text, "\n")
	var out []SubEntry
	inProxies := false
	// baseIndent 「- 」列表项的缩进；块式条目的字段缩进必须比它深。
	baseIndent := -1
	var cur map[string]string
	var curLine int
	var curName string

	flush := func() {
		if cur == nil {
			return
		}
		out = append(out, clashEntryToSubEntry(curLine, curName, cur))
		cur = nil
	}

	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if !inProxies {
			if isClashProxiesKey(line) {
				// proxies 的值是流式序列时整段取出来单独解析（可能同一行，也可能跨行：
				//   proxies: [{...}, {...}]
				//   proxies: [
				//     {...},
				//   ]
				// ）。块式写法（下一行起 "- name: ..."）走下面的逐行状态机，两条路径
				// 互斥，不会重复解析。
				if val, vl, ok := flowSequenceValue(lines, i); ok {
					if !bracketsBalanced(val) {
						// 未闭合的数组按错误上报：静默返回 0 条会让用户以为订阅是空的。
						return nil, fmt.Errorf("proxies 的流式数组未闭合（第 %d 行起）", vl)
					}
					return append(out, parseFlowArray(vl, val)...), nil
				}
				inProxies = true
			}
			continue
		}
		// 段结束：出现非列表项、且缩进不比列表项深的 key（如 proxy-groups:）。
		if !strings.HasPrefix(trimmed, "- ") && trimmed != "-" {
			if baseIndent < 0 || indent <= baseIndent {
				flush()
				inProxies = false
				continue
			}
			// 块式条目的字段行。
			if cur != nil {
				if k, v, ok := splitYAMLKV(trimmed); ok {
					cur[k] = v
				}
			}
			continue
		}
		// 新的列表项。
		flush()
		if baseIndent < 0 {
			baseIndent = indent
		}
		item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		curLine = lineNo
		cur = map[string]string{}
		curName = ""
		if strings.HasPrefix(item, "{") {
			// 流式写法：整条在行内。
			cur = parseFlowMap(item)
			curName = cur["name"]
			flush()
			continue
		}
		if k, v, ok := splitYAMLKV(item); ok {
			cur[k] = v
			if k == "name" {
				curName = v
			}
		}
	}
	flush()
	return out, nil
}

// flowSequenceValue 取出 `proxies:` 的流式序列值（`[...]`，含跨行形式），并返回其
// 起始行号（1 起）。值不是流式序列（块式 `- name: ...`）时 ok=false，调用方走原来的
// 逐行状态机。
//
// 三种真实写法都要认：
//
//	proxies: [{name: a, ...}, {name: b, ...}]
//	proxies: [
//	  {name: a, ...},
//	]
//	proxies:
//	  [{name: a, ...}]
func flowSequenceValue(lines []string, keyIdx int) (string, int, bool) {
	rest := strings.TrimSpace(valueAfterColon(lines[keyIdx]))
	if rest == "" {
		// 值在后续行：跳过空行与注释行，看第一行是不是 "["。
		for j := keyIdx + 1; j < len(lines); j++ {
			t := strings.TrimSpace(strings.TrimRight(lines[j], "\r"))
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if !strings.HasPrefix(t, "[") {
				return "", 0, false
			}
			return collectFlowValue(lines, j, t), j + 1, true
		}
		return "", 0, false
	}
	if !strings.HasPrefix(rest, "[") {
		return "", 0, false
	}
	return collectFlowValue(lines, keyIdx, rest), keyIdx + 1, true
}

// collectFlowValue 从 start 行开始累积，直到方括号配平（跨行时按空格拼接，跳过纯注释行）。
func collectFlowValue(lines []string, start int, first string) string {
	buf := first
	if bracketsBalanced(buf) {
		return buf
	}
	for j := start + 1; j < len(lines); j++ {
		t := strings.TrimSpace(strings.TrimRight(lines[j], "\r"))
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		buf += " " + t
		if bracketsBalanced(buf) {
			break
		}
	}
	return buf
}

// valueAfterColon 取 `key: value` 里冒号之后的部分（未去引号；只用于判形态）。
func valueAfterColon(line string) string {
	i := strings.Index(line, ":")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[i+1:])
}

// bracketsBalanced 报告 [ ] 与 { } 是否配平，忽略引号内的括号（名字/密码里出现
// 括号是常见的，按字符数数必然误判）。
func bracketsBalanced(s string) bool {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		}
	}
	return depth == 0 && quote == 0
}

// parseFlowArray 解析 `[ {...}, {...} ]` 形式的流式数组。
func parseFlowArray(lineNo int, s string) []SubEntry {
	inner := strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(s), "]"), "["))
	var out []SubEntry
	for _, part := range splitFlowItems(inner) {
		m := parseFlowMap(part)
		if len(m) == 0 {
			continue
		}
		out = append(out, clashEntryToSubEntry(lineNo, m["name"], m))
	}
	return out
}

// splitFlowItems 按顶层逗号切分流式写法（引号内、花括号内的逗号不切）。
func splitFlowItems(s string) []string {
	var parts []string
	var buf strings.Builder
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && i+1 < len(s) {
				buf.WriteByte(c)
				i++
				c = s[i]
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, buf.String())
			buf.Reset()
			continue
		}
		buf.WriteByte(c)
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		parts = append(parts, s)
	}
	return parts
}

// parseFlowMap 把 `{k: v, k2: v2}`（或去掉花括号的 `k: v, k2: v2`）解析成 map。
func parseFlowMap(s string) map[string]string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	out := map[string]string{}
	for _, part := range splitFlowItems(s) {
		if k, v, ok := splitYAMLKV(part); ok {
			out[k] = v
		}
	}
	return out
}

// splitYAMLKV 切 `key: value`，去掉值两端引号。没有冒号或键为空时 ok=false。
func splitYAMLKV(s string) (string, string, bool) {
	s = strings.TrimSpace(s)
	i := strings.Index(s, ":")
	if i <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(s[:i])
	v := strings.TrimSpace(s[i+1:])
	if k == "" {
		return "", "", false
	}
	// 去掉 yaml 注释（值里的人名/密码带 # 的情况极少，且带引号时下面的 Trim 会先处理）。
	if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
		v = v[1 : len(v)-1]
	}
	return strings.ToLower(k), v, true
}

// clashEntryToSubEntry 把一条 Clash 代理定义转成受支持的代理 URL，或给出不支持的说明。
func clashEntryToSubEntry(line int, name string, m map[string]string) SubEntry {
	kind := strings.ToLower(strings.TrimSpace(m["type"]))
	e := SubEntry{Line: line, Name: name, Kind: kind}
	if kind == "" {
		e.Reason = "缺少 type 字段，无法判定协议"
		return e
	}
	if knownUnsupportedSchemes[kind] {
		e.Unsupported = true
		e.Reason = unsupportedReason(kind)
		return e
	}
	if _, ok := supportedProxySchemes[kind]; !ok {
		e.Reason = "未知的代理类型 " + kind + "（本网关只支持 http/https/socks5）"
		return e
	}
	host := strings.TrimSpace(m["server"])
	if host == "" {
		e.Reason = "缺少 server 字段"
		return e
	}
	port, err := strconv.Atoi(strings.TrimSpace(m["port"]))
	if err != nil || port < 1 || port > 65535 {
		e.Reason = "port 字段缺失或不是 1-65535 的整数"
		return e
	}
	u := &url.URL{Scheme: kind, Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if user := strings.TrimSpace(m["username"]); user != "" {
		u.User = url.UserPassword(user, strings.TrimSpace(m["password"]))
	}
	e.URL = u.String()
	return e
}

// parseURILines 明文 URI 列表：一行一个。
func parseURILines(text string) ([]SubEntry, error) {
	lines := strings.Split(text, "\n")
	out := make([]SubEntry, 0, len(lines))
	for i, raw := range lines {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		out = append(out, parseURILine(i+1, line))
	}
	return out, nil
}

// parseURILine 归一一行 URI。socks:// 是 socks5 的常见别名，按 socks5 处理；
// 其余不支持的协议给出具体原因。
func parseURILine(line int, s string) SubEntry {
	scheme := ""
	if i := strings.Index(s, "://"); i > 0 {
		scheme = strings.ToLower(s[:i])
	}
	e := SubEntry{Line: line, Kind: scheme}
	if scheme == "" {
		e.Reason = "不是代理 URL（缺少 scheme://）；本框也接受 Clash/subscription 全文"
		return e
	}
	if knownUnsupportedSchemes[scheme] {
		e.Unsupported = true
		e.Reason = unsupportedReason(scheme)
		return e
	}
	switch scheme {
	case "socks", "socks5", "socks5h", "http", "https":
	default:
		e.Reason = "未知的协议 " + scheme + "（本网关只支持 http/https/socks5）"
		return e
	}
	u, err := url.Parse(s)
	if err != nil || u == nil {
		e.Reason = "无法解析的代理 URL"
		return e
	}
	if scheme == "socks" {
		u.Scheme = "socks5"
		e.Kind = "socks5"
	}
	// 名字（#fragment）只作展示：parseURL 明确拒绝带 fragment 的 URL，这里剥掉。
	if u.Fragment != "" {
		e.Name = u.Fragment
		u.Fragment, u.RawFragment = "", ""
	}
	e.URL = u.String()
	return e
}

func unsupportedReason(kind string) string {
	return kind + " 协议不由本网关解析（传输层只支持 http/https/socks5）；" +
		"可在本机另起一个 mihomo 导出 socks5 端口，再把那个 socks5:// 地址导入"
}
