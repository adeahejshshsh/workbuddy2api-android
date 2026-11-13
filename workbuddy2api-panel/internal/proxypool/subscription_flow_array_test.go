package proxypool

import (
	"strings"
	"testing"
)

// 流式序列作为 proxies 的值：这是 YAML 合法写法，此前只认字面量 "[]"，于是整段
// 落到 URI 行解析 —— 一行认不出来，产出一个 name/kind/url 全空的占位条目，节点被
// 静默丢掉（与 hub PR #858 报的「只认块式写法、遇到流式就静默出错」同一类缺陷）。
func TestParseSubscriptionClashFlowArray(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		want  []string // 期望的 URL 序列
		names []string
	}{
		{
			name:  "单行流式数组",
			yaml:  `proxies: [{name: "HK 01", type: socks5, server: hk.example.com, port: 1080}, {name: "JP 01", type: http, server: jp.example.com, port: 3128}]` + "\n",
			want:  []string{"socks5://hk.example.com:1080", "http://jp.example.com:3128"},
			names: []string{"HK 01", "JP 01"},
		},
		{
			name: "跨行流式数组",
			yaml: `mixed-port: 7890
proxies: [
  {name: "HK 01", type: socks5, server: hk.example.com, port: 1080},
  {name: "JP 01", type: socks5, server: jp.example.com, port: 1080},
]
rules:
  - MATCH,DIRECT
`,
			want:  []string{"socks5://hk.example.com:1080", "socks5://jp.example.com:1080"},
			names: []string{"HK 01", "JP 01"},
		},
		{
			name: "数组在键的下一行",
			yaml: `proxies:
  [{name: A, type: socks5, server: a.example.com, port: 1080},
   {name: B, type: socks5, server: b.example.com, port: 1080}]
`,
			want:  []string{"socks5://a.example.com:1080", "socks5://b.example.com:1080"},
			names: []string{"A", "B"},
		},
		{
			name:  "尾随逗号与引号内的方括号",
			yaml:  `proxies: [{name: "带]括号", type: socks5, server: a.example.com, port: 1080}, {name: B, type: socks5, server: b.example.com, port: 1080},]` + "\n",
			want:  []string{"socks5://a.example.com:1080", "socks5://b.example.com:1080"},
			names: []string{"带]括号", "B"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries, err := ParseSubscription(c.yaml)
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if len(entries) != len(c.want) {
				t.Fatalf("got %d 条 %+v，want %d", len(entries), entries, len(c.want))
			}
			for i, e := range entries {
				if e.URL != c.want[i] {
					t.Errorf("第 %d 条 URL=%q want %q", i+1, e.URL, c.want[i])
				}
				if e.Name != c.names[i] {
					t.Errorf("第 %d 条 name=%q want %q", i+1, e.Name, c.names[i])
				}
				// 空条目是这次要消灭的形态：type 与 URL 同时为空说明整行没被认出。
				if e.Kind == "" || e.URL == "" {
					t.Errorf("第 %d 条解析成空条目: %+v", i+1, e)
				}
			}
		})
	}
}

// 空数组仍然是「0 条、不报错」（原有行为，别被流式分支改坏）。
func TestParseSubscriptionClashFlowArrayEmpty(t *testing.T) {
	for _, yaml := range []string{"proxies: []\n", "proxies:\n  []\n"} {
		entries, err := ParseSubscription(yaml)
		if err != nil {
			t.Fatalf("%q: err=%v", yaml, err)
		}
		if len(entries) != 0 {
			t.Fatalf("%q: got %d 条", yaml, len(entries))
		}
	}
}

// 未闭合的流式数组要报错，不能静默返回 0 条——否则用户以为订阅里没有节点。
func TestParseSubscriptionClashFlowArrayUnclosed(t *testing.T) {
	yaml := "proxies: [\n  {name: A, type: socks5, server: a.example.com, port: 1080},\n"
	_, err := ParseSubscription(yaml)
	if err == nil {
		t.Fatal("未闭合的流式数组应报错")
	}
	if !strings.Contains(err.Error(), "未闭合") {
		t.Fatalf("错误文案应说明未闭合，got %v", err)
	}
}

// 块式写法必须完全不受影响（流式分支不能抢走块式输入）。
func TestParseSubscriptionClashBlockUnaffected(t *testing.T) {
	yaml := `proxies:
  - name: HK 01
    type: socks5
    server: hk.example.com
    port: 1080
  - {name: FLOW, type: socks5, server: f.example.com, port: 1080}
proxy-groups:
  - name: 自动选择
    type: url-test
    proxies: [HK 01]
rules:
  - MATCH,DIRECT
`
	entries, err := ParseSubscription(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d 条: %+v", len(entries), entries)
	}
	if entries[0].URL != "socks5://hk.example.com:1080" || entries[1].URL != "socks5://f.example.com:1080" {
		t.Fatalf("块式与行内条目混排解析错: %+v", entries)
	}
}

// bracketsBalanced 的引号处理：名字里带方括号不应被判成未配平。
func TestBracketsBalanced(t *testing.T) {
	cases := map[string]bool{
		`[{a: 1}]`:        true,
		`[{a: "]"}]`:      true,
		`[{a: 1},`:        false,
		`[{a: "未闭合}`:      false,
		`[]`:              true,
		`[{name: 'x'} ]`:  true,
		"[\n  {a: 1},\n]": true,
	}
	for in, want := range cases {
		if got := bracketsBalanced(in); got != want {
			t.Errorf("bracketsBalanced(%q)=%v want %v", in, got, want)
		}
	}
}
