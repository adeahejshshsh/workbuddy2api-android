package server

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 客户端先断开时不得报成「疑似上游断流」，也不得再发一次注定失败的续写。
//
// 实测踩过：CodeBuddy 在 189s 后放弃，上游被动 cancel，日志记成「疑似上游断流」
// 把排障引向错误方向；另一条 seg=1 续写失败也是 ctx 已取消后白发的请求。
func TestContinueClientGoneNotUpstreamCut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &continueReader{ctx: ctx, acct: &auth.Auth{UID: "u"}, body: []byte(`{"max_tokens":100}`)}
	r.text.WriteString("partial")
	cancel()

	if !r.clientGone() {
		t.Fatal("ctx 已取消时应判定客户端断开")
	}
	if r.canContinue(false) {
		t.Fatal("客户端断开后不该再发续写请求")
	}
}

// cutThenErr 先吐完 data 再返回 err：模拟流被掐断。
type cutThenErr struct {
	r   io.Reader
	err error
}

func (c *cutThenErr) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		return n, c.err
	}
	return n, err
}

func (c *cutThenErr) Close() error { return nil }

// 读错误同为 context canceled 时，只有客户端自己的请求 ctx 取消才算「客户端先断开」。
// 上游静默超时被空闲看门狗（upstream/idle.go）掐断时，取消的是派生的上游请求 ctx，
// 客户端还在等：仍按上游断流处理，照常盲重试一次。
func TestContinueClientGoneOnlyByClientContext(t *testing.T) {
	cut := `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"半截正文"}}]}` + "\n\n"
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	acct := &auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999}
	for _, tc := range []struct {
		name        string
		clientGone  bool
		wantRetries int
	}{
		{"idle watchdog cut, client still waiting", false, 1},
		{"client disconnected", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, reqs := fakeContinueUpstream(t, func(int) (int, string) { return 200, testSeg2Text })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.clientGone {
				cancel()
			}
			rc := &cutThenErr{r: strings.NewReader(cut), err: context.Canceled}
			r := newContinueReader(ctx, up, acct, []byte(body), "1.2.3.4", upstream.ChatMeta{}, rc)
			out, _ := io.ReadAll(r)
			if len(*reqs) != tc.wantRetries {
				t.Fatalf("续写请求 %d 次，want %d", len(*reqs), tc.wantRetries)
			}
			if tc.clientGone && strings.Contains(string(out), `"finish_reason":"length"`) {
				t.Fatalf("客户端已断开不该补截断终态:\n%s", out)
			}
		})
	}
}
