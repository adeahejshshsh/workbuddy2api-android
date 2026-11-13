package proxypool

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestParseSubscriptionClashBlock 块式 Clash 配置：只取 proxies: 段，其余段忽略。
// 「只取 proxies:」是硬要求——proxy-groups 里也常有 name/type 字段，整篇当 YAML 跑
// 会把代理组当成节点导进来。
func TestParseSubscriptionClashBlock(t *testing.T) {
	yaml := `mixed-port: 7890
allow-lan: false
proxies:
  - name: 香港 01
    type: socks5
    server: hk1.example.com
    port: 1080
    username: user1
    password: pass1
  - name: 直连 HTTP
    type: http
    server: 10.0.0.5
    port: 3128
  - name: 不支持的
    type: ss
    server: ss.example.com
    port: 8388
    cipher: aes-256-gcm
    password: x
proxy-groups:
  - name: 自动选择
    type: url-test
    proxies: [香港 01]
rules:
  - MATCH,DIRECT
`
	entries, err := ParseSubscription(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("应解析出 3 条（proxy-groups 不算），got %d: %+v", len(entries), entries)
	}
	if entries[0].URL != "socks5://user1:pass1@hk1.example.com:1080" {
		t.Errorf("第一条 URL=%q", entries[0].URL)
	}
	if entries[0].Name != "香港 01" {
		t.Errorf("名字应保留空格与中文，got %q", entries[0].Name)
	}
	if entries[1].URL != "http://10.0.0.5:3128" {
		t.Errorf("无凭据的 http 节点 URL=%q", entries[1].URL)
	}
	if entries[2].URL != "" || entries[2].Kind != "ss" || entries[2].Reason == "" {
		t.Errorf("ss 应被标为不支持并给出原因，got %+v", entries[2])
	}
}

// TestParseSubscriptionClashFlow 流式写法（真实订阅里非常常见）。
func TestParseSubscriptionClashFlow(t *testing.T) {
	yaml := `proxies:
  - {name: "A, 带逗号", type: socks5, server: a.example.com, port: 1080}
  - {name: B, type: https, server: b.example.com, port: 443, username: u, password: "p:w"}
  - {name: C, type: vmess, server: c.example.com, port: 443}
`
	entries, err := ParseSubscription(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d: %+v", len(entries), entries)
	}
	// 引号内的逗号不能当分隔符，否则这条会被切成两半。
	if entries[0].Name != "A, 带逗号" || entries[0].URL != "socks5://a.example.com:1080" {
		t.Errorf("名字里带逗号被切坏了: %+v", entries[0])
	}
	// 密码里的冒号要保留（splitYAMLKV 只按第一个冒号切）。
	if entries[1].URL != "https://u:p%3Aw@b.example.com:443" {
		t.Errorf("密码含冒号: %q", entries[1].URL)
	}
	if entries[2].Kind != "vmess" || entries[2].URL != "" {
		t.Errorf("vmess 应被标为不支持: %+v", entries[2])
	}
}

// TestParseSubscriptionBase64Body 整段 base64 订阅：先解码再按明文 URI 处理。
func TestParseSubscriptionBase64Body(t *testing.T) {
	plain := "socks5://u:p@hk.example.com:1080#香港\nhttp://10.0.0.5:3128#内网\n"
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		body := enc.EncodeToString([]byte(plain))
		entries, err := ParseSubscription(body)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 {
			t.Fatalf("%v: got %d: %+v", enc, len(entries), entries)
		}
		if entries[0].URL != "socks5://u:p@hk.example.com:1080" {
			t.Errorf("base64 解码后 URL=%q", entries[0].URL)
		}
		// #fragment 只作名字，不能留在 URL 里（parseURL 会拒绝带 fragment 的 URL）。
		if entries[0].Name != "香港" {
			t.Errorf("fragment 应作为名字: %q", entries[0].Name)
		}
	}
}

// TestParseSubscriptionPlainURIs 明文列表：socks:// 是 socks5 的常见别名；
// 不支持的协议给出具体原因而不是笼统的「格式错误」。
func TestParseSubscriptionPlainURIs(t *testing.T) {
	body := strings.Join([]string{
		"# 注释行不算条目",
		"",
		"socks://user:pass@hk.example.com:1080",
		"http://10.0.0.5:3128",
		"trojan://pass@t.example.com:443#T",
		"垃圾内容",
	}, "\n")
	entries, err := ParseSubscription(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("注释与空行应跳过，got %d: %+v", len(entries), entries)
	}
	if entries[0].Kind != "socks5" || entries[0].URL != "socks5://user:pass@hk.example.com:1080" {
		t.Errorf("socks:// 应归一成 socks5: %+v", entries[0])
	}
	if entries[1].URL != "http://10.0.0.5:3128" {
		t.Errorf("http: %+v", entries[1])
	}
	if entries[2].Kind != "trojan" || entries[2].URL != "" || !strings.Contains(entries[2].Reason, "mihomo") {
		t.Errorf("trojan 应给出可操作的原因: %+v", entries[2])
	}
	if entries[3].Kind != "" || !strings.Contains(entries[3].Reason, "scheme") {
		t.Errorf("认不出的行应报 scheme 缺失: %+v", entries[3])
	}
}

// TestParseSubscriptionEmpty 空输入返回空列表而不是错误：面板的空提交不该变成 500。
func TestParseSubscriptionEmpty(t *testing.T) {
	for _, body := range []string{"", "   \n\t\n"} {
		entries, err := ParseSubscription(body)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if len(entries) != 0 {
			t.Fatalf("%q: got %d", body, len(entries))
		}
	}
}

// TestImportSubscriptionEndToEnd 端到端：Clash 订阅直接进池，unsupported 条目逐条报账
// 而不是静默丢弃；重复导入同一条 URL 判 duplicate（订阅每 6 小时拉一次是常态）。
func TestImportSubscriptionEndToEnd(t *testing.T) {
	m, err := New("", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	yaml := `proxies:
  - {name: 港1, type: socks5, server: hk1.example.com, port: 1080}
  - {name: 港2, type: http, server: hk2.example.com, port: 3128}
  - {name: 港3, type: ss, server: hk3.example.com, port: 8388}
`
	rows, err := m.Import(yaml)
	if err != nil {
		t.Fatal(err)
	}
	var added, unsupported int
	for _, r := range rows {
		switch r.Status {
		case "added":
			added++
		case "unsupported":
			unsupported++
			if r.Name != "港3" || r.Error == "" {
				t.Errorf("unsupported 行应带名字与原因: %+v", r)
			}
		}
	}
	if added != 2 || unsupported != 1 {
		t.Fatalf("added=%d unsupported=%d，行: %+v", added, unsupported, rows)
	}
	// 节点名取自订阅（不是 host），面板才认得出是哪个节点。
	names := map[string]bool{}
	for _, p := range m.Snapshot().Proxies {
		names[p.Name] = true
	}
	if !names["港1"] || !names["港2"] {
		t.Fatalf("节点名应取订阅里的 name，got %v", names)
	}
	// 再导一次：同 URL 全部 duplicate，不产生新节点。
	rows2, err := m.Import(yaml)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows2 {
		if r.Status == "added" {
			t.Fatalf("重复导入不该新增节点: %+v", r)
		}
	}
	if got := len(m.Snapshot().Proxies); got != 2 {
		t.Fatalf("池里应仍只有 2 个节点，got %d", got)
	}
}

// TestParseSubscriptionKeepsLineNumbers 行号必须对应**原文**的行（含空行）。
//
// 回归背景：ParseSubscription 起初对整段输入做了 TrimSpace，于是首行是空行的输入
// 整体前移一行，第 2 行被报成第 1 行。面板靠 r.line 索引回填输入框（把无效行原样
// 放回文本框让用户改），错位会把**上一行**放回去——用户改完再提交，改的是另一行。
func TestParseSubscriptionKeepsLineNumbers(t *testing.T) {
	body := "\nHTTP://a.example.com:80/\r\nbad://x\n\nsocks5://b.example.com:1080\n"
	entries, err := ParseSubscription(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("空行应跳过但行号要保留，got %d: %+v", len(entries), entries)
	}
	want := []int{2, 3, 5}
	for i, w := range want {
		if entries[i].Line != w {
			t.Fatalf("第 %d 条行号=%d want %d（原文行号）", i, entries[i].Line, w)
		}
	}
}
