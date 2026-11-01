// headorder.go 出站请求头**发送顺序**对齐官方客户端。
//
// 背景：Go 的 net/http 写头时按 map key 排序（Header.writeSubset 的
// sortedKeyValues），而官方客户端是 Node/undici/axios，按**代码插入序**写出。
// 头名的集合与拼写已由 headcase.go 对齐，本文件补最后一项差异：顺序。
//
// 依据（真机夹具，逐字节原文）：
//
//	testdata/official_chat_head.txt        43 头，chat 链
//	testdata/official_report_cn_head.txt   11 头，/v2/report
//	testdata/official_chat_busy_head.txt    8 头，/v2/chat/busy
//	testdata/official_v3config_head.txt     4 头，/v3/config
//
// 顺序表按**路径**分族：官方不同链路由不同代码构造头对象，顺序本就不通用，
// 拿 chat 的顺序套 report 反而更假。表里没有的路径保持 Go 原生排序——不做
// 无证据的猜测。
package upstream

import (
	"bytes"
	"strings"
)

// officialHeadOrder 各主链路的官方头顺序（原文大小写，抄自夹具）。
//
// 末尾的 Host / Connection / Content-Length / Accept-Encoding 是 net/http
// 自己追加的，重排时一并纳入，位置按真机。
var officialHeadOrder = map[string][]string{
	"/v2/chat/completions": {
		"Accept",
		"Content-Type",
		"X-Requested-With",
		"x-stainless-arch",
		"x-stainless-lang",
		"x-stainless-os",
		"x-stainless-package-version",
		"x-stainless-retry-count",
		"x-stainless-runtime",
		"x-stainless-runtime-version",
		"X-Conversation-ID",
		"X-Agent-Purpose",
		"X-IDE-Type",
		"X-IDE-Name",
		"X-IDE-Version",
		"X-Agent-Intent",
		"X-Agent-Runtime",
		"X-Private-Data",
		"acp-connection-id",
		"x-codebuddy-request",
		"X-Request-ID",
		"X-Conversation-Message-ID",
		"X-Conversation-Request-ID",
		"X-Root-Request-ID",
		"X-Agent-Type",
		"baggage",
		"Content-Encoding",
		"traceparent",
		"b3",
		"X-B3-TraceId",
		"X-B3-ParentSpanId",
		"X-B3-SpanId",
		"X-B3-Sampled",
		"X-Trace-ID",
		"X-Product",
		"User-Agent",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
	"/v2/report": {
		"Accept",
		"Content-Type",
		"X-Request-ID",
		"X-Product",
		"User-Agent",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
	"/v2/chat/busy": {
		"Accept",
		"Content-Type",
		"User-Agent",
		"X-Request-ID",
		"X-Product",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Host",
		"Connection",
	},
	"/v3/config": {
		"Accept",
		"Connection",
		"User-Agent",
		"X-Request-ID",
		"X-Product",
		"Host",
	},
}

// headRank 把顺序表压成「小写头名 → 名次」，比对时大小写不敏感
// （Go transport 写出的 key 已是官方拼写，但 Host 之类仍是 canonical）。
func headRank(path string) (map[string]int, bool) {
	order, ok := officialHeadOrder[path]
	if !ok {
		return nil, false
	}
	rank := make(map[string]int, len(order))
	for i, k := range order {
		rank[strings.ToLower(k)] = i
	}
	return rank, true
}

// requestPathOf 从请求行取 path（去掉 query），用于查顺序表。
func requestPathOf(line string) string {
	// METHOD SP request-target SP HTTP/x.y
	sp1 := bytes.IndexByte([]byte(line), ' ')
	if sp1 <= 0 {
		return ""
	}
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return ""
	}
	target := rest[:sp2]
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	// absolute-form（http://host/path）时只留 path
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if i := strings.Index(target[8:], "/"); i >= 0 {
			target = target[8+i:]
		}
	}
	return target
}

// reorderHeadBlock 按官方顺序重排一个完整的请求头块（含结尾空行）。
//
// 行为：
//   - 顺序表里有的头按表排；
//   - 表里没有的头（上游新增、临时注入）保持**原有相对顺序**追加在末尾；
//   - 重复头名（极少）保持各自出现顺序；
//   - 无法识别的请求行 / 空表 → 原样返回，绝不抛错。
//
// 纯函数，不依赖任何连接状态，是这套机制唯一需要精确正确的地方。
func reorderHeadBlock(block []byte) []byte {
	if len(block) == 0 || !bytes.HasSuffix(block, []byte("\r\n\r\n")) {
		return block
	}
	body := block[:len(block)-4] // 去掉终止空行
	lines := strings.Split(string(body), "\r\n")
	if len(lines) < 2 {
		return block
	}
	path := requestPathOf(lines[0])
	rank, ok := headRank(path)
	if !ok {
		return block
	}

	type entry struct {
		name string
		raw  string
		seq  int
	}
	var entries []entry
	for i, ln := range lines[1:] {
		if ln == "" {
			continue
		}
		colon := strings.IndexByte(ln, ':')
		if colon <= 0 {
			continue
		}
		entries = append(entries, entry{name: ln[:colon], raw: ln, seq: i})
	}

	// 稳定排序：有名次的按名次，没名次的排后面并保持原序
	ordered := make([]entry, 0, len(entries))
	used := make([]bool, len(entries))
	for want := 0; want < len(rank); want++ {
		for i, e := range entries {
			if used[i] {
				continue
			}
			if r, ok := rank[strings.ToLower(e.name)]; ok && r == want {
				ordered = append(ordered, e)
				used[i] = true
			}
		}
	}
	for i, e := range entries {
		if !used[i] {
			ordered = append(ordered, e)
		}
	}

	var out bytes.Buffer
	out.Grow(len(block))
	out.WriteString(lines[0])
	out.WriteString("\r\n")
	for _, e := range ordered {
		out.WriteString(e.raw)
		out.WriteString("\r\n")
	}
	out.WriteString("\r\n")
	return out.Bytes()
}
