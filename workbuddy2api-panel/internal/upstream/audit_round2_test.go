// audit_round2_test.go 代码审计第二轮（A1-02 / W4-U1-02 / W4-U2-002 及其 P2 同族）
// 的回归用例。每个用例的注释都写清「改之前是什么行为、为什么那是错的」，
// 避免以后有人把缺陷当成契约再改回去。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestRefreshHeadersSnapshotKeepsRealm A1-02（P1）回归。
//
// 缺陷：refreshTokenOnce 用「显式逐字段拷贝」构造 auth.Auth 快照去造头，而
// auth.Auth.realm 是包内 unexported 字段 —— 包外赋值编译不过，于是快照的 realm
// 恒为空。Realm() 只能按 Domain 后缀推断，**Domain 为空的 realm=global 账号**
// 就整体退化成 CN 指纹（Origin/Referer=codebuddy.cn、UA 品牌 WorkBuddy、
// Accept-Language=zh-CN），refresh 请求打到 global base 却带着 CN 来源域。
//
// 断言：global 账号（realm 显式 global、Domain 空）的 refresh 请求必须带
// global 的 Origin/Referer、UA 品牌段与 en-US。
func TestRefreshHeadersSnapshotKeepsRealm(t *testing.T) {
	var gotOrigin, gotReferer, gotUA, gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin = r.Header.Get("Origin")
		gotReferer = r.Header.Get("Referer")
		gotUA = r.Header.Get("User-Agent")
		gotLang = r.Header.Get("Accept-Language")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`))
	}))
	defer srv.Close()

	a := &auth.Auth{UID: "u-global", AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: 1}
	// 显式落 realm=global 但**不给 Domain** —— 这正是退化成 CN 指纹的形态。
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatalf("seed realm: %v", err)
	}
	if a.Realm() != "global" {
		t.Fatalf("前置条件失败：账号应为 global，得到 %q", a.Realm())
	}

	c := &Client{
		HTTP: srv.Client(), GlobalEnabled: true,
		ChatBaseGlobal: srv.URL, ChatBaseCN: "http://127.0.0.1:1",
	}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}

	if gotOrigin != "https://www.workbuddy.ai" {
		t.Fatalf("A1-02: global 账号 refresh 的 Origin = %q，应为 https://www.workbuddy.ai（CN 指纹说明快照丢了 realm）", gotOrigin)
	}
	if gotReferer != "https://www.workbuddy.ai/" {
		t.Fatalf("A1-02: Referer = %q", gotReferer)
	}
	if !strings.Contains(gotUA, "workbuddy-ai/") {
		t.Fatalf("A1-02: UA = %q，global 账号的 UA 品牌段应为 workbuddy-ai", gotUA)
	}
	if gotLang != "en-US" {
		t.Fatalf("A1-02: Accept-Language = %q，global 账号应为 en-US", gotLang)
	}
}

// TestRefreshHeadersSnapshotKeepsCNRealm 反向守卫：CN 账号不能被上面的修复带跑。
func TestRefreshHeadersSnapshotKeepsCNRealm(t *testing.T) {
	var gotOrigin, gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin = r.Header.Get("Origin")
		gotLang = r.Header.Get("Accept-Language")
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at-new","refreshToken":"rt-new"}}`))
	}))
	defer srv.Close()

	a := &auth.Auth{UID: "u-cn", AccessToken: "at-old", RefreshToken: "rt-old", Domain: "www.codebuddy.cn"}
	if _, err := auth.BackfillRealmFor(a, "cn"); err != nil {
		t.Fatalf("seed realm: %v", err)
	}
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if gotOrigin != "https://www.codebuddy.cn" || gotLang != "zh-CN" {
		t.Fatalf("CN 账号头被改错：Origin=%q Accept-Language=%q", gotOrigin, gotLang)
	}
}

// TestReadLineLimitedCapsLine W4-U1-02 / F-11（P1）单元回归。
//
// 缺陷：bufio.Reader.ReadString('\n') 的 64KB 只约束内部读块，**返回的行本身没有
// 上限**。一条不含换行的超长行会让内存按行长线性增长直到 OOM（进程级，不是请求级）。
func TestReadLineLimitedCapsLine(t *testing.T) {
	// 上限内：行为必须与 ReadString 逐字一致（含行尾 \n）。
	br := bufio.NewReaderSize(strings.NewReader("data: 1\nrest"), 16)
	line, err := readLineLimited(br, 64)
	if err != nil || line != "data: 1\n" {
		t.Fatalf("常规行被改变：line=%q err=%v", line, err)
	}

	// 超限：必须返回 errSSELineTooLong，且**不能**把超长内容交出来。
	long := strings.Repeat("a", 4096)
	br = bufio.NewReaderSize(strings.NewReader(long+"\nnext-line\n"), 16)
	line, err = readLineLimited(br, 128)
	if line != "" {
		t.Fatalf("超长行被返回了 %d 字节", len(line))
	}
	if !IsSSELineTooLong(err) {
		t.Fatalf("应为 errSSELineTooLong，得到 %v", err)
	}
	// 排空校验：下一次读必须从**行边界**开始，否则后续每一行都会错位。
	line, err = readLineLimited(br, 128)
	if err != nil || line != "next-line\n" {
		t.Fatalf("超长行未排空（后续行错位）：line=%q err=%v", line, err)
	}

	// EOF 无换行：与 ReadString 一致，返回残留 + io.EOF。
	br = bufio.NewReaderSize(strings.NewReader("tail-no-newline"), 16)
	line, err = readLineLimited(br, 64)
	if line != "tail-no-newline" || err == nil {
		t.Fatalf("EOF 残留语义不一致：line=%q err=%v", line, err)
	}
}

// TestAggregateRejectsOverlongLine W4-U1-02 的非流式（Aggregate）出口回归。
// 超长行必须变成显式的 upstream read 错误（→502 stream_error），
// 而不是静默截断后当「零有效事件」或半截 JSON 处理。
func TestAggregateRejectsOverlongLine(t *testing.T) {
	input := "data: " + strings.Repeat("x", MaxSSELineBytes+1024) + "\n\n"
	_, err := Aggregate(strings.NewReader(input))
	if err == nil {
		t.Fatal("超长行必须报错，不能当空流/成功处理")
	}
	if !IsSSELineTooLong(err) {
		t.Fatalf("错误链里应能看出是超长行（IsSSELineTooLong），得到 %v", err)
	}
	if !IsUpstreamReadError(err) {
		t.Fatalf("应包成 errUpstreamRead 让 handler 走 502 stream_error，得到 %v", err)
	}
}

// TestStreamHintRejectsOverlongLine 流式出口回归：超长行必须变成**显式失败**。
//
// 分两段断言，因为 StreamHint 有「首帧否决」语义：
//  1. 首帧前就超长 → 一个字节都没写，返回 ErrStreamBrokeBeforeFlush（可换号重试，
//     不能把 200 钉死）；
//  2. 已有有效帧之后超长 → 补 error 帧 + [DONE] 收尾并返回 errUpstreamRead，
//     客户端明确看到「上游发了畸形帧」，而不是拿到一条被静默截断的流。
func TestStreamHintRejectsOverlongLine(t *testing.T) {
	long := "data: " + strings.Repeat("y", MaxSSELineBytes+1024) + "\n\n"

	// 1) 首帧前超长。
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(long), nil)
	if err == nil || !errors.Is(err, ErrStreamBrokeBeforeFlush) {
		t.Fatalf("首帧前超长应返回 ErrStreamBrokeBeforeFlush（可换号），得到 %v", err)
	}

	// 2) 已有有效帧后超长。
	good := "data: " + `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"
	rec = httptest.NewRecorder()
	err = StreamHint(rec, strings.NewReader(good+long), nil)
	if err == nil || !IsUpstreamReadError(err) {
		t.Fatalf("流式路径应把超长行报成 upstream read 错误，得到 %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("收尾必须补 [DONE]（客户端不能悬挂）：%q", body)
	}
	if !strings.Contains(body, "upstream_read_error") {
		t.Fatalf("必须写出显式 error 帧，不能静默结束：%q", body)
	}
	if strings.Contains(body, strings.Repeat("y", 1024)) {
		t.Fatal("超长行内容被写给了客户端（说明没被拦住）")
	}
}

// TestRepairDeferredArgsTruncatedInStringNotRepaired W4-U2-002（P1）回归。
//
// 缺陷：closeTruncatedJSON 对「截断在字符串内部」也补一个引号，把不完整参数变成
// 合法 JSON 下发。调用方拿到的是「合法 JSON」，无从区分「上游给的就是这个半截值」
// 与「被截断了」，于是 WebFetch 访问半截 URL、写文件工具写错路径 —— 静默产生
// 错误副作用。正确行为：字符串内截断 = 值不可知 → 放弃修复，保留原文。
func TestRepairDeferredArgsTruncatedInStringNotRepaired(t *testing.T) {
	raw := `{"name":"WebFetch","input":"{\"url\":\"https://example.com/pa`
	got, kind := repairDeferredArgs(raw)
	if kind != "" || got != raw {
		t.Fatalf("字符串内截断被「修」成了合法 JSON（半截 url 会被当真值执行）：kind=%q got=%q", kind, got)
	}

	// 容器边界上的截断仍然要修（缺的只是闭合括号，值本身完整）。
	okRaw := `{"name":"WebFetch","input":{"url":"https://example.com/a","prompt":"hi"}`
	got, kind = repairDeferredArgs(okRaw)
	if kind != "truncation" {
		t.Fatalf("容器边界截断应仍可修，kind=%q", kind)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("修出来的不是合法 JSON：%v (%s)", err, got)
	}
}

// TestCloseTruncatedJSONStringTailRejected 直接钉死 closeTruncatedJSON 的边界：
// 只有「字符串已闭合」才允许补括号。
func TestCloseTruncatedJSONStringTailRejected(t *testing.T) {
	for _, in := range []string{
		`{"a":"b`,
		`{"a":"b\`,
		`{"url":"https://x/pa`,
		`[{"q":"abc`,
	} {
		if got, ok := closeTruncatedJSON(in); ok {
			t.Fatalf("W4-U2-002：字符串内截断被补成 %q（应放弃修复）", got)
		}
	}
	// 值已闭合、只缺括号：仍应修好。
	for _, in := range []string{`{"a":"b"`, `[{"a":1}`, `{"a":[]`} {
		got, ok := closeTruncatedJSON(in)
		if !ok {
			t.Fatalf("容器边界截断应可修：%q", in)
		}
		var v any
		if err := json.Unmarshal([]byte(got), &v); err != nil {
			t.Fatalf("修出来不是合法 JSON：%q → %q (%v)", in, got, err)
		}
	}
}

// TestReadLineLimitedErrorIsClassifiable 保证错误可被 errors.Is 识别（避免上层
// 只能靠字符串匹配归因）。
func TestReadLineLimitedErrorIsClassifiable(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("z", 512)+"\n"), 8)
	_, err := readLineLimited(br, 64)
	if !errors.Is(err, errSSELineTooLong) {
		t.Fatalf("errors.Is 应命中 errSSELineTooLong，得到 %v", err)
	}
}

// ───────────────────────── 第二轮 P2 回归 ─────────────────────────

// boomBody 读即报错的 body（模拟 deflate 读失败）。名字带 boom 前缀避免与
// audit_p2_test.go 的 errReadCloser 撞名。
type boomBody struct{ err error }

func (e boomBody) Read([]byte) (int, error) { return 0, e.err }
func (e boomBody) Close() error             { return nil }

// TestDecodeBodyDeflateReadErrorNotSilent A1-05（P2，剩余面）回归。
//
// 缺陷：deflate 分支先把 resp.Body 读进内存再 Close；读失败时**原样 return resp**，
// 此时 body 已关闭且半消费，调用方 io.ReadAll 拿到 0 字节 + nil 错误，而
// doJSONRaw 把「200 + 空 body」当成功 —— 一个解不开的 deflate 响应被记成
// 「成功但没数据」，错误彻底消失。
func TestDecodeBodyDeflateReadErrorNotSilent(t *testing.T) {
	wantErr := errors.New("boom")
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"deflate"}},
		Body:   boomBody{err: wantErr},
	}
	out := decodeBodyIfNeeded(resp)
	_, rerr := io.ReadAll(out.Body)
	if rerr == nil {
		t.Fatal("A1-05：deflate 读失败必须沿 ReadAll 上抛，不能变成「0 字节 + nil」（会被当成空体成功）")
	}
	if !errors.Is(rerr, wantErr) {
		t.Fatalf("错误链应保留底层原因，得到 %v", rerr)
	}
}

// TestDecodeBodyDeflateOverLimitNotSilent 超限同样必须显式报错（不能退化成空体成功）。
func TestDecodeBodyDeflateOverLimitNotSilent(t *testing.T) {
	big := bytes.Repeat([]byte{0}, int(deflateMaxBytes)+16)
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"deflate"}},
		Body:   io.NopCloser(bytes.NewReader(big)),
	}
	out := decodeBodyIfNeeded(resp)
	_, rerr := io.ReadAll(out.Body)
	if rerr == nil {
		t.Fatal("deflate 超限必须报错")
	}
	if !errors.Is(rerr, ErrResponseTooLarge) {
		t.Fatalf("超限应能看出是网关上限（ErrResponseTooLarge），得到 %v", rerr)
	}
}

// TestReadDeviceTokenFileNoLockAroundIO A1-06（P2）回归。
//
// 缺陷：读文件全程持 dtFileCache.mu（defer Unlock）。这是每个出站请求都会走的
// 路径，宿主把 token 文件放慢盘/NFS 时，一次 stat+read 卡顿会阻塞**全部**
// chat/billing 出站。这里用一个「慢 Close/慢 Read 不可控」的替代验证方式：
// 在 IO 期间尝试拿同一把锁，必须能拿到（说明锁没被 IO 占住）。
func TestReadDeviceTokenFileNoLockAroundIO(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "device_token")
	if err := os.WriteFile(fp, []byte("tok-1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	save := resetDeviceTokenFileCache(fp)
	defer save()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = readDeviceTokenFile(fp)
	}()
	// 等 IO 开始（命中缓存前的必经路径），随后尝试抢锁：
	// 若实现仍是「锁内 IO」，这里会阻塞到 IO 完成；给 50ms 窗口足够暴露。
	time.Sleep(10 * time.Millisecond)
	locked := make(chan struct{})
	go func() {
		dtFileCache.mu.Lock()
		dtFileCache.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("A1-06：文件 IO 期间锁被长期占住（慢盘会阻塞全部出站请求）")
	}
	<-done
	if got := readDeviceTokenFile(fp); got != "tok-1" {
		t.Fatalf("token = %q", got)
	}
}

// failConn 写必失败的 net.Conn（测 io.Writer 契约）。
type failConn struct {
	net.Conn
	writes int
}

func (c *failConn) Write(p []byte) (int, error) { c.writes++; return 0, errors.New("write failed") }
func (c *failConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *failConn) Close() error                { return nil }

// TestHeadOrderConnWriteReportsConsumed A1-08（P2）回归。
//
// 缺陷：Write 已把 p 收进 c.pending（等于已消费）后底层写失败仍 return 0, err。
// io.Writer 契约里 0 意味着「一个字节都没消费」，transport 会据此重发整块 ——
// 把已经写出的请求头再写一遍，连接当场错位。
func TestHeadOrderConnWriteReportsConsumed(t *testing.T) {
	head := "GET /x HTTP/1.1\r\nHost: h\r\nAccept: */*\r\n\r\n"
	fc := &failConn{}
	c := newHeadOrderConn(fc)
	n, err := c.Write([]byte(head))
	if err == nil {
		t.Fatal("底层写失败必须返回错误")
	}
	if n != len(head) {
		t.Fatalf("A1-08：已消费语义要求返回 len(p)=%d，得到 %d（0 会让 transport 重发整块）", len(head), n)
	}
}

// TestAggregateMultilineData F-04 / W4-U1-03（P2）回归。
//
// 缺陷：Aggregate 把每个 data: 行当独立帧解析，SSE 规范允许的多行 data 拼帧
// 完全不支持 —— 美化/折行的 JSON 每行都解析失败被静默丢弃，validEvents==0 →
// errEmptyStream（502 upstream_parse），而同一条流在流式路径是正常的。
func TestAggregateMultilineData(t *testing.T) {
	full := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
		`"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`
	// 真实形态是**美化输出**：JSON 本身带换行，上游按 SSE 规范把每一行各发一个
	// data: 前缀。拼回来时用 "\n" 连接正好还原原文。
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(full), "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	var b strings.Builder
	for _, ln := range strings.Split(pretty.String(), "\n") {
		b.WriteString("data: " + ln + "\n")
	}
	input := b.String() + "\n" + "data: [DONE]\n\n"
	out, err := Aggregate(strings.NewReader(input))
	if err != nil {
		t.Fatalf("多行 data 不该报错（F-04）：%v", err)
	}
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if got := msg["content"]; got != "hello" {
		t.Fatalf("多行 data 内容丢失：content=%q", got)
	}
}

// TestAggregateDoneWithExtraSpaces F-23（P3，同批）回归：`data:  [DONE]` 也要收尾。
func TestAggregateDoneWithExtraSpaces(t *testing.T) {
	frame := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`
	out, err := Aggregate(strings.NewReader("data: " + frame + "\n\ndata:  [DONE]\n\n"))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	c := out["choices"].([]any)[0].(map[string]any)
	if fr, _ := c["finish_reason"].(string); fr != "stop" {
		t.Fatalf("finish_reason 被改写成 %q（说明 [DONE] 没被认出来 → 假截断）", fr)
	}
}

// TestStreamFlushDataBeforeTerminalFrame F-05 / W4-U1-04（P2）回归。
//
// 缺陷：流尾「补截断终态」与「空流错误帧」都写在 flushDataLines 之前，客户端
// 先收到 finish_reason=length、**随后**又收到正文帧（回合结束后还有内容），
// 严格客户端会丢弃后半段。终态必须永远是最后一帧。
func TestStreamFlushDataBeforeTerminalFrame(t *testing.T) {
	// 一条永远拼不成合法 JSON 的多行 data，随后 EOF。
	input := "data: {\"choices\":[\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"x\"}}\n\n"
	rec := httptest.NewRecorder()
	_ = StreamHint(rec, strings.NewReader(input), nil)
	body := rec.Body.String()
	lastFinish := strings.LastIndex(body, `"finish_reason"`)
	lastData := strings.LastIndex(body, `data: {"choices":[`)
	if lastFinish >= 0 && lastData > lastFinish {
		t.Fatalf("F-05：正文帧出现在终态帧之后（客户端会丢弃）：\n%s", body)
	}
}

// TestTrimRoundsKeepsBigInt W4-U1-34 / F-07（P2）回归：裁剪后大整数必须逐字保留。
func TestTrimRoundsKeepsBigInt(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":"old"},` +
		`{"role":"assistant","content":"old-a"},` +
		`{"role":"user","content":"keep","meta":1234567890123456789},` +
		`{"role":"assistant","content":"keep-a"}]}`
	out := TrimRounds([]byte(body), 1)
	if !strings.Contains(string(out), "1234567890123456789") {
		t.Fatalf("W4-U1-34：大整数被 float64 改写：%s", out)
	}
	if strings.Contains(string(out), "e+") {
		t.Fatalf("W4-U1-34：出现科学计数法：%s", out)
	}
}

// TestTrimRoundsNoHTMLEscape W4-U1-33（P2）回归：不能把 < > & 转成 \u003c。
func TestTrimRoundsNoHTMLEscape(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":"old"},` +
		`{"role":"user","content":"a <tool> & b"}]}`
	out := TrimRounds([]byte(body), 1)
	if strings.Contains(string(out), `\u003c`) || strings.Contains(string(out), `\u0026`) {
		t.Fatalf("W4-U1-33：HTML 转义把出站字节改了（指纹面）：%s", out)
	}
	if !strings.Contains(string(out), "a <tool> & b") {
		t.Fatalf("内容被改写：%s", out)
	}
}

// TestPrepareKimiToolsKeepsBigInt F-07（P2）回归：tools schema 里的大整数不能失真。
func TestPrepareKimiToolsKeepsBigInt(t *testing.T) {
	body := `{"model":"kimi-k2","tools":[{"type":"function","function":{"name":"f",` +
		`"parameters":{"type":"object","properties":{"id":{"type":"integer","minimum":1234567890123456789}}}}}]}`
	out := prepareKimiTools([]byte(body), "cn")
	if !strings.Contains(string(out), "1234567890123456789") {
		t.Fatalf("F-07：tools 里的大整数被改写：%s", out)
	}
}

// TestProxyHandshakeDeadlineApplied F-09 / W4-U2-030（P2）回归：
// 握手阶段必须给连接装 deadline（否则代理不回包时 dial 线程永久挂住）。
func TestProxyHandshakeDeadlineApplied(t *testing.T) {
	// deadlineConn 记录 SetDeadline 调用。
	dc := &deadlineConn{}
	clear := armProxyHandshakeDeadline(context.Background(), dc)
	if !dc.armed {
		t.Fatal("F-09：握手阶段没有设置 deadline")
	}
	clear()
	if !dc.cleared {
		t.Fatal("F-09：返回前必须把 deadline 清 0（否则长连接被 10s 误伤）")
	}
}

type deadlineConn struct {
	net.Conn
	armed, cleared bool
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	if t.IsZero() {
		c.cleared = true
	} else {
		c.armed = true
	}
	return nil
}

// TestTranslateMaxTokensNullTreatedAsAbsent W4-U1-13（P2）回归。
//
// 缺陷：翻译守卫是「键是否存在」，`{"max_tokens":null,"max_completion_tokens":1000}`
// 键存在 → 不翻译；收尾删除守卫用的是 hasPositiveInt（null 不是有效值）→ 别名也不删。
// 结果 body 里同时带 null 的 max_tokens 和 1000 的别名，上游只读前者 → 按默认上限截断。
func TestTranslateMaxTokensNullTreatedAsAbsent(t *testing.T) {
	obj := map[string]any{"max_tokens": nil, "max_completion_tokens": json.Number("1000")}
	translateMaxCompletionTokens(obj)
	if _, has := obj["max_completion_tokens"]; has {
		t.Fatal("W4-U1-13：别名没被翻译/删除")
	}
	// 出站字节才是唯一口径：断言 marshal 后 max_tokens 是 1000（翻译可能写 int64
	// 也可能写 json.Number，两者 marshal 出来都是裸数字）。
	raw, err := marshalNoEscape(obj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"max_tokens":1000`) {
		t.Fatalf("W4-U1-13：max_tokens 没被翻译成 1000：%s", raw)
	}
}

// TestDropReasoningPartsNonAssistantNotLeftEmpty W4-U1-18（P2）回归。
//
// 缺陷：所有角色一律 `content = ""`。user 消息的 content 全是 reasoning 片段时，
// 出站多一条**空 content 的 user 消息** → 上游 11151/400，日志无痕迹。
func TestDropReasoningPartsNonAssistantNotLeftEmpty(t *testing.T) {
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "reasoning_text", "text": "think"},
		}},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "reasoning_text", "text": "think"},
		}},
	}}
	dropReasoningContentParts(obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("user 的空内容消息应整条丢弃，剩 %d 条", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "assistant" {
		t.Fatalf("留下的应是 assistant，得到 %v", m["role"])
	}
	if m["content"] != "" {
		t.Fatalf("assistant 轮应保留并置空 content，得到 %#v", m["content"])
	}
}

// TestBackfillReasoningIgnoresNonAssistantTrace W4-U1-22（P2）回归。
//
// 缺陷：hasTrace 不区分角色、也不看值是否为空 —— user 消息带一个空的
// reasoning_content 键就会让「disabled + 无痕迹」的请求被误判成有痕迹，
// 于是每条 assistant 都被塞进 " " 占位（凭空往上下文里造内容）。
func TestBackfillReasoningIgnoresNonAssistantTrace(t *testing.T) {
	obj := map[string]any{
		"thinking": map[string]any{"type": "disabled"},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi", "reasoning_content": ""},
			map[string]any{"role": "assistant", "content": "yo"},
		},
	}
	backfillReasoningContent(obj)
	msg := obj["messages"].([]any)[1].(map[string]any)
	if _, has := msg["reasoning_content"]; has {
		t.Fatalf("W4-U1-22：disabled + 无痕迹时不该回填，得到 %#v", msg["reasoning_content"])
	}
}

// TestSanitizeToolArgumentsNoZeroWidth W4-U2-016（P2）回归。
//
// 缺陷：品牌词替换（插 U+200B）也作用在 function.arguments 上 —— 工具参数是会被
// 执行的数据，凭空多出不可见字符会让落盘内容/URL/正则全部错位。
func TestSanitizeToolArgumentsNoZeroWidth(t *testing.T) {
	calls := []any{map[string]any{"function": map[string]any{
		"name":      "Write",
		"arguments": `{"path":"a.txt","content":"DeepSeek Harness"}`,
	}}}
	if changed := sanitizeToolCalls(calls, true); changed {
		fn := calls[0].(map[string]any)["function"].(map[string]any)
		if strings.Contains(fn["arguments"].(string), "\u200b") {
			t.Fatalf("W4-U2-016：参数里被插入零宽空格：%q", fn["arguments"])
		}
	}
}

// TestContainsFoldASCIINoAlloc W4-U2-017（P2）回归：大小写折叠不得整串分配。
func TestContainsFoldASCIINoAlloc(t *testing.T) {
	text := strings.Repeat("x", 1<<16) + "Powered By" + strings.Repeat("y", 1<<16)
	allocs := testing.AllocsPerRun(20, func() {
		if !containsFoldASCII(text, "powered by") {
			t.Fatal("应命中")
		}
	})
	if allocs > 0 {
		t.Fatalf("W4-U2-017：containsFoldASCII 仍有分配（%.1f 次/op），等于每条消息多一份等长副本", allocs)
	}
	if containsFoldASCII("abc", "abcd") {
		t.Fatal("长度不足时应返回 false")
	}
}

// TestModelCatalogPutManySavesOnce W4-U2-047（P2）回归：
// 批量回流只落盘一次（原来 N 个模型 → N 次全量序列化 + N 次持锁写）。
func TestModelCatalogPutManySavesOnce(t *testing.T) {
	resetModelCatalog()
	defer resetModelCatalog()
	path := filepath.Join(t.TempDir(), "model.json")
	SetModelCatalogPath(path)

	batch := map[string]ModelCapEntry{}
	for _, m := range []string{"a", "b", "c", "d"} {
		batch[m] = ModelCapEntry{ContextLength: 1000, MaxOutputTokens: 100, Source: "modelsdev"}
	}
	catalogState.putMany(batch)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("批量写入必须落盘：%v", err)
	}
	for _, m := range []string{"a", "b", "c", "d"} {
		if _, ok := modelCatalogGet(m); !ok {
			t.Fatalf("%s 没写进缓存", m)
		}
		if !strings.Contains(string(raw), `"`+m+`"`) {
			t.Fatalf("%s 没落盘：%s", m, raw)
		}
	}
}

// TestVerifySessionByRefreshPersistsRotatedRT W4-U2-093（P2）回归。
//
// 缺陷：RefreshToken 会轮换 RT 但只写内存，而 VerifySessionByRefresh 把落盘声明成
// 「调用方职责」。任何调用点漏了落盘，进程重启后读回的是已被上游作废的旧 RT ——
// 健康账号变成永久不可刷新，而本次判词仍是 VerifyAlive，从返回值看不出来。
func TestVerifySessionByRefreshPersistsRotatedRT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"accessToken":"at-new","refreshToken":"rt-rotated","expiresIn":3600}}`))
	}))
	defer srv.Close()

	fp := filepath.Join(t.TempDir(), "acc.json")
	a := &auth.Auth{UID: "u-verify", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: 1, FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if v := c.VerifySessionByRefresh(a); v != VerifyAlive {
		t.Fatalf("verdict=%v", v)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	if !strings.Contains(string(raw), "rt-rotated") {
		t.Fatalf("W4-U2-093：轮换后的 RT 没落盘（重启即真死）:\n%s", raw)
	}
}

// TestMachineIDForStablePerUID W4-U2-082（P2）回归。
//
// 缺陷：mpEventBase 的 machineId 是硬编码常量 —— 整池账号上报同一台「设备」，
// 正是上游按设备维度批量封号的典型特征。要求：同账号恒同（幂等）、跨账号不同。
func TestMachineIDForStablePerUID(t *testing.T) {
	a := machineIDFor("u-1")
	if a != machineIDFor("u-1") {
		t.Fatal("同账号必须稳定（重放一致性）")
	}
	if a == machineIDFor("u-2") {
		t.Fatal("不同账号必须不同（否则仍是全池共用一台设备）")
	}
	if a == "0655736a-607f-4d9d-b430-58176ee9a090" {
		t.Fatal("还是那个硬编码常量")
	}
	// UUID 形状 8-4-4-4-12
	parts := strings.Split(a, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 ||
		len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("machineId 不是 UUID 形状：%q", a)
	}
	if got, _ := mpEventBase(&auth.Auth{UID: "u-1"})["machineId"].(string); got != a {
		t.Fatalf("mpEventBase 没接上派生值：%q", got)
	}
}

// blockUntilCloseBody Read 一直阻塞到 Close 被调用（模拟「Close 才打断 Read」的底流）。
type blockUntilCloseBody struct {
	closed chan struct{}
	once   sync.Once
	order  *[]string
	mu     *sync.Mutex
}

func (b *blockUntilCloseBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockUntilCloseBody) Close() error {
	b.mu.Lock()
	*b.order = append(*b.order, "close")
	b.mu.Unlock()
	b.once.Do(func() { close(b.closed) })
	return nil
}

// TestIdleCloseClosesBodyBeforeCancel A1-09（P2）回归。
//
// 缺陷：Close 的读尾 goroutine 超时后，旧实现先 cancel() 再 rc.Close()。cancel 只
// 作用于 HTTP 请求的 context，**打不断**包在中间的任意 io.ReadCloser；唯一能打断
// 阻塞 Read 的动作是 Close。顺序反了就等于让那个 goroutine 继续挂在 Read 上，
// 长期运行就是 goroutine 与 fd 的缓慢累积。
func TestIdleCloseClosesBodyBeforeCancel(t *testing.T) {
	var order []string
	var mu sync.Mutex
	under := &blockUntilCloseBody{closed: make(chan struct{}), order: &order, mu: &mu}
	// idle 设很小，但 Close 走的是 tailDrainWait 分支（读尾会阻塞）——
	// 这里直接把读尾卡住，验证超时分支的先后顺序。
	b := monitorBody(under, time.Hour, func() {
		mu.Lock()
		order = append(order, "cancel")
		mu.Unlock()
	})
	start := time.Now()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close 阻塞了 %v（读尾超时没有兜住）", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "close" || order[1] != "cancel" {
		t.Fatalf("A1-09：必须先 Close 再 cancel，实际顺序 %v", order)
	}
}

// TestContextWindowNegativeCacheStillTriggersFetch W4-U2-053（P2）回归。
//
// 缺陷：负缓存为真时连 ensureDocAsync 都不调 —— 首次拉取失败（或拉取之前就 miss
// 过）之后，该模型 24h 内再也不会触发文档拉取，context_length 固定在 1M 兜底，
// 即使 5min 冷却早已过去。ensureDocAsync 自带冷却与 in-flight 去重，重复调用廉价。
//
// 观测方式：ensureDocAsync 在起 goroutine 之前就把 fetched 置真；传 nil client
// 让它立刻返回（不打真网）。
func TestContextWindowNegativeCacheStillTriggersFetch(t *testing.T) {
	resetModelsDev()
	defer resetModelsDev()
	if _, ok := modelsDev.lookup("never-heard-of-it"); ok {
		t.Fatal("前置条件：不该命中")
	}
	if !modelsDev.negativeFresh("never-heard-of-it") {
		t.Fatal("前置条件：应已进负缓存")
	}
	got := ContextWindowListingV4("never-heard-of-it", 0, nil)
	if got != DefaultContextWindow {
		t.Fatalf("兜底值应为 1M，得到 %d", got)
	}
	modelsDev.mu.Lock()
	fetched := modelsDev.fetched
	modelsDev.mu.Unlock()
	if !fetched {
		t.Fatal("W4-U2-053：负缓存把文档拉取也短路了（冷却过去也不会再拉）")
	}
}
