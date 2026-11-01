// official_fixture_test.go 官方客户端抓包夹具对拍（审计用，默认跳过）。
//
// 跑法：WB2A_PARITY=1 go test ./internal/upstream -run TestOfficialParity -v
//
// 夹具 testdata/official_*_head.txt 是 **WorkBuddy 官方桌面端真机抓包**的
// 原始请求头原文（保序、保大小写、已脱敏）。本测试做三件事：
//
//  1. 用生产代码构造同路径请求，经裸 TCP 抓真实出站字节；
//  2. 从字节里解析头名（Go 的 http.Header 拿不到原始大小写，必须看 wire）；
//  3. 与夹具逐名比对：集合 / 大小写 / 静态值，输出差异表。
//
// 它不做断言门禁——差异本身就是审计结论，由人决定改不改。
// 默认跳过，不污染常规 CI。
package upstream

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// officialCase 一张夹具对应的生产代码调用方式。
type officialCase struct {
	fixture string // testdata/official_xxx_head.txt
	path    string // 对照的请求路径（夹具第一行里的）
	chat    bool   // 走 chatHTTP（SSE 那条）而不是 HTTP
	build   func(c *Client, a *auth.Auth, base string) *http.Request
	// ignoreValue 该路径上**刻意偏离**的头（只豁免值比对，不豁免存在性）。
	// 每一笔都必须写明为什么 —— 这是给自己放水的唯一入口，滥用它等于把门禁关掉。
	ignoreValue map[string]bool
}

// dynamicHeader 值每次不同、不能逐一比对（只比存在性与形态）。
var dynamicHeader = map[string]bool{
	"x-request-id": true, "x-conversation-message-id": true,
	"x-conversation-request-id": true, "x-root-request-id": true,
	"x-conversation-id": true, "acp-connection-id": true,
	"traceparent": true, "b3": true, "x-b3-traceid": true,
	"x-b3-parentspanid": true, "x-b3-spanid": true, "x-trace-id": true,
	"baggage": true, "authorization": true, "x-user-id": true,
	// connection **不在此列**：它的值由 path 决定（keep-alive / close），是官方
	// 固定发送的字段而非传输层动态产物。豁免它等于让「官方必发、我们从没发过」
	// 这类回归永远不会被门禁抓到 —— 本项目已经踩过一次（56f479d）。
	"content-length": true, "host": true,
	"content-encoding": true, "cookie": true, "x-device-token": true,
	// x-domain 从上游 base 推出来，测试里 base 是 127.0.0.1 抓包监听，
	// 生产环境才等于真实域；这条差异是脚手架造成的，不算产品缺陷。
	"x-domain": true,
}

// transportHeader 传输层头，不进内容比对。
var transportHeader = map[string]bool{
	"host": true, "content-length": true,
	"accept-encoding": true, "content-encoding": true,
}

func loadFixture(t *testing.T, name string) (order []string, values map[string]string) {
	t.Helper()
	fp := filepath.Join("testdata", name)
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读夹具失败 %s: %v", fp, err)
	}
	values = map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	first := true
	for sc.Scan() {
		ln := strings.TrimRight(sc.Text(), "\r")
		if ln == "" {
			continue
		}
		if first {
			first = false
			continue // 请求行
		}
		i := strings.Index(ln, ":")
		if i <= 0 {
			continue
		}
		k := ln[:i]
		v := strings.TrimSpace(ln[i+1:])
		order = append(order, k)
		values[k] = v
	}
	return
}

// parseRawHeaderNames 从原始请求字节里取头名（保大小写、保顺序）。
func parseRawHeaderNames(raw string) (order []string, values map[string]string) {
	values = map[string]string{}
	for _, ln := range strings.Split(raw, "\r\n") {
		i := strings.Index(ln, ":")
		if i <= 0 {
			continue
		}
		k := ln[:i]
		if strings.HasPrefix(strings.ToUpper(ln), "HTTP/") {
			continue
		}
		order = append(order, k)
		values[k] = strings.TrimSpace(ln[i+1:])
	}
	return
}

// lookupVal 大小写不敏感地取值（wire 上的大小写随发送方，不能假定）。
func lookupVal(values map[string]string, key string) string {
	for k, v := range values {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// TestOfficialParity 逐路径对拍官方真机头与本项目出站头。
//
// 两种模式：
//
//	WB2A_PARITY=report  只打印差异表，不判失败（审计用）
//	WB2A_PARITY=1       打印 + **断言差异为零**（CI 门禁用）
//
// 默认跳过，不污染常规 go test。
func TestOfficialParity(t *testing.T) {
	mode := os.Getenv("WB2A_PARITY")
	if mode == "" {
		t.Skip("set WB2A_PARITY=1 (gate) 或 WB2A_PARITY=report (audit) to run")
	}
	strict := mode != "report"

	cn := &auth.Auth{UID: "uid-fixture", AccessToken: "AT", RefreshToken: "RT",
		Nickname: "fixture", Domain: "www.workbuddy.cn"}
	meta := ChatMeta{
		ConversationID:        "f7c6c392-e016-43bb-beca-ab918b436668",
		ConversationRequestID: "01a194ad31c27d81804ee9915cf954fb",
		CacheKeyID:            "sess-fixture",
	}

	cases := []officialCase{
		{
			fixture: "official_chat_head.txt", path: "/v2/chat/completions", chat: true,
			build: func(c *Client, a *auth.Auth, base string) *http.Request {
				req, _ := http.NewRequest(http.MethodPost, base+"/v2/chat/completions",
					strings.NewReader(`{"model":"x","messages":[]}`))
				c.ChatHeaders(req, a, "", meta)
				return req
			},
		},
		{
			fixture: "official_chat_busy_head.txt", path: "/v2/chat/busy",
			build: func(c *Client, a *auth.Auth, base string) *http.Request {
				// 真走生产函数 ModelBusy：手工拼头会漏掉它注入的身份头，
				// 那是脚手架缺陷、会被误读成产品漏发。这里用 ChatBaseCN 把
				// 上游指到抓包监听上，由 ModelBusy 自己构造请求。
				c.ChatBaseCN = base
				_, _ = c.ModelBusy(a, []string{"hy3"})
				return nil // ModelBusy 已经发过请求
			},
		},
		{
			fixture: "official_v3config_head.txt", path: "/v3/config",
			// 本仓 /v3/config 的 UA 是刻意偏离：走 IDE UA + CLI UA 双路探测取并集，
			// 实测两路各有独有模型（见 codeBuddyCLIUA 注释）；改成官方两段式会丢模型。
			ignoreValue: map[string]bool{"user-agent": true},
			build: func(c *Client, a *auth.Auth, base string) *http.Request {
				req, _ := http.NewRequest(http.MethodGet, base+"/v3/config", nil)
				req.Header.Set("Accept", "application/json, text/plain, */*")
				// 本仓 /v3/config 的 UA 是**刻意偏离**：我们走「IDE UA + CLI UA 双路探测
				// 取并集」，实测两路各有独有模型（见 codeBuddyCLIUA 注释）；改成官方
				// 两段式会丢模型。故这里用我方真实 UA，并跳过与夹具的逐字节比对。
				req.Header.Set("User-Agent", c.userAgent(a))
				req.Header.Set("X-Request-ID", "x")
				req.Header.Set("X-Product", "SaaS")
				return req
			},
		},
	}

	for _, tc := range cases {
		t.Run(strings.TrimSuffix(strings.TrimPrefix(tc.fixture, "official_"), "_head.txt"), func(t *testing.T) {
			offOrder, offVals := loadFixture(t, tc.fixture)

			var raw string
			c := probeClient()
			raw = rawCaptured(t, false, func(base string) {
				req := tc.build(c, cn, base)
				if req == nil {
					return // build 自己发了请求（如 ModelBusy）
				}
				// 必须经 do()：它是唯一出站口，头名拼写纠正（normalizeHeaderSpelling）
				// 就在里面。直接 c.HTTP.Do() 会绕过它，看到的不是生产真实字节。
				if tc.chat {
					c.ChatBaseCN = base
				}
				resp, err := c.do(req)
				if err != nil {
					return
				}
				resp.Body.Close()
			})
			if i := strings.Index(raw, "\r\n\r\n"); i >= 0 {
				raw = raw[:i]
			}
			gotOrder, gotVals := parseRawHeaderNames(raw)
			if len(gotOrder) == 0 {
				t.Fatalf("未抓到出站请求字节（raw=%q）", raw[:min(len(raw), 120)])
			}

			// 1) 集合差异（小写归并，跳过传输层头）
			offSet, gotSet := map[string]string{}, map[string]string{}
			for _, k := range offOrder {
				if !transportHeader[strings.ToLower(k)] {
					offSet[strings.ToLower(k)] = k
				}
			}
			for _, k := range gotOrder {
				if !transportHeader[strings.ToLower(k)] {
					gotSet[strings.ToLower(k)] = k
				}
			}
			var missing, extra []string
			for k, orig := range offSet {
				if _, ok := gotSet[k]; !ok {
					missing = append(missing, orig)
				}
			}
			for k, orig := range gotSet {
				if _, ok := offSet[k]; !ok {
					extra = append(extra, orig)
				}
			}

			// 2) 大小写差异
			var caseDiff []string
			for k, orig := range offSet {
				if g, ok := gotSet[k]; ok && g != orig {
					caseDiff = append(caseDiff, orig+"  ←项目 "+g)
				}
			}

			// 3) 静态值差异
			var valDiff []string
			for k, orig := range offSet {
				if dynamicHeader[k] {
					continue
				}
				g, ok := gotSet[k]
				if !ok {
					continue
				}
				if offVals[orig] != gotVals[g] {
					valDiff = append(valDiff, k+": 官方="+strconv.Quote(offVals[orig])+" 项目="+strconv.Quote(gotVals[g]))
				}
			}

			t.Logf("\n===== %s =====", tc.fixture)
			t.Logf("官方 %d 头 / 项目 %d 头（不含传输层）", len(offSet), len(gotSet))
			if len(missing) > 0 {
				t.Logf("【漏发】%v", missing)
			}
			if len(extra) > 0 {
				t.Logf("【多发】%v", extra)
			}
			if len(caseDiff) > 0 {
				t.Logf("【大小写不同 %d 项】", len(caseDiff))
				for _, d := range caseDiff {
					t.Logf("   %s", d)
				}
			}
			if len(tc.ignoreValue) > 0 {
				kept := valDiff[:0]
				for _, d := range valDiff {
					skip := false
					for h := range tc.ignoreValue {
						if strings.Contains(strings.ToLower(d), strings.ToLower(h)+":") {
							skip = true
							t.Logf("   [刻意偏离，已豁免] %s", d)
						}
					}
					if !skip {
						kept = append(kept, d)
					}
				}
				valDiff = kept
			}
			if len(valDiff) > 0 {
				t.Logf("【值不同】")
				for _, d := range valDiff {
					t.Logf("   %s", d)
				}
			}
			total := len(missing) + len(extra) + len(caseDiff) + len(valDiff)
			if total == 0 {
				t.Logf("完全一致")
			}

			// Accept-Encoding 单独判定：它是可指纹化的头，但有两个已知边界——
			//   1. chat 链路有意保留 Go 自动 gzip（SSE 流解压风险，见 ChatHeaders）；
			//   2. 真机某些路径（chat/busy、v3/config）没有这个头，而 Go 一定会补
			//      gzip，除非全局 DisableCompression——那会把所有链路的透明解压关掉。
			// 所以只对「真机有值」的路径做相等断言，chat 允许降级成 gzip。
			offAE := lookupVal(offVals, "Accept-Encoding")
			gotAE := lookupVal(gotVals, "Accept-Encoding")
			t.Logf("Accept-Encoding: 官方=%q 项目=%q", offAE, gotAE)

			if strict {
				if total > 0 {
					t.Errorf("%s：与官方真机存在 %d 处差异（漏发%d/多发%d/大小写%d/值%d）",
						tc.fixture, total, len(missing), len(extra), len(caseDiff), len(valDiff))
				}
				if offAE != "" {
					wantAE := offAE
					if tc.chat {
						// 已知残留：chat 交回 net/http 自动 gzip。
						wantAE = "gzip"
					}
					if gotAE != wantAE {
						t.Errorf("%s：Accept-Encoding=%q want %q", tc.fixture, gotAE, wantAE)
					}
				}
			}
		})
	}
}
