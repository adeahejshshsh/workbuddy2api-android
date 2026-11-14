package proxypool

import "testing"

// Clash 块式条目的三处形态：tls: true 的 http 节点是 HTTPS 代理；嵌套列表（alpn）与嵌套映射里的
// 「- 」和同名键不是新条目、也不盖掉外层字段；值后的行尾注释不是值的一部分。
func TestParseClashBlockShapes(t *testing.T) {
	text := `proxies:
  - name: tls-http
    type: http
    server: a.example.com
    port: 443 # 主线路
    username: "u#1"
    password: p#2
    tls: true
    alpn:
      - h2
      - http/1.1
    ws-opts:
      server: wrong.example.com
  - name: plain
    type: socks5
    server: b.example.com
    port: 1080
  - name: socks-tls
    type: socks5
    server: c.example.com
    port: 1080
    tls: true
proxy-groups:
  - name: g
    type: select
`
	got, err := ParseSubscription(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(got), got)
	}
	if got[0].URL != "https://u%231:p%232@a.example.com:443" {
		t.Errorf("tls http entry = %+v", got[0])
	}
	if got[1].URL != "socks5://b.example.com:1080" || got[1].Name != "plain" {
		t.Errorf("plain entry = %+v", got[1])
	}
	if got[2].URL != "" || !got[2].Unsupported {
		t.Errorf("socks5 over tls must not degrade to plaintext: %+v", got[2])
	}
}
