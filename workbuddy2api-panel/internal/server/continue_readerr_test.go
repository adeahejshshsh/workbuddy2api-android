package server

// 本仓特有的一条接线约束：continueReader 把上游读错误吞掉（因为多数形态要降级成
// 截断终态），但「一个字节都没写出去」的那一次尝试对客户端完全不可见 —— 此时必须把
// 原始错误继续往上冒，StreamHint 的首帧否决与 handler 的换号才有依据。
//
// 去掉这条冒泡会立刻把两个既有用例打红（TestStreamRetriesWhenUpstreamBrokeBeforeFirstFrame
// / TestStreamEarlyBreakReturns502JSONWhenRetriesExhausted）：上游断在首帧前的请求
// 会被伪装成「正常收尾的空流」，客户端拿到 200 + 空流而不是换号重试 / 502 JSON。

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// errReader 首次 Read 就返回指定错误（模拟 200 之后立刻断流）。
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestContinueReaderBubblesReadErrorWhenNothingWritten(t *testing.T) {
	up, _ := fakeContinueUpstream(t, func(int) (int, string) { return 200, testSeg2Text })
	acct := &auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999}
	want := io.ErrUnexpectedEOF
	r := newContinueReader(context.Background(), up, acct, []byte(`{"model":"m","messages":[]}`),
		"1.2.3.4", upstream.ChatMeta{}, io.NopCloser(errReader{err: want}))

	if _, err := io.ReadAll(r); !errors.Is(err, want) {
		t.Fatalf("err=%v want %v（零字节写出时读错误必须冒泡，否则换号重试失效）", err, want)
	}
}

func TestContinueReaderSwallowsReadErrorAfterOutput(t *testing.T) {
	up, _ := fakeContinueUpstream(t, func(int) (int, string) { return 200, testSeg2Text })
	acct := &auth.Auth{UID: "u1", AccessToken: "at-1", ExpiresAt: 9999999999}
	// 段 1：先吐一帧正文，然后断流（无 finish、无 [DONE]）。
	seg := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"半句"}}]}` + "\n\n"
	r := newContinueReader(context.Background(), up, acct, []byte(`{"model":"m","messages":[]}`),
		"1.2.3.4", upstream.ChatMeta{}, io.NopCloser(io.MultiReader(strings.NewReader(seg), errReader{err: io.ErrUnexpectedEOF})))

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("写过字节后不该把读错误冒给上层: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "半句") {
		t.Fatalf("缺首段正文:\n%s", s)
	}
	// 断流盲重试会把续写段接上，最终以 stop 收尾（不是 length）。
	if !strings.Contains(s, `"finish_reason":"stop"`) {
		t.Fatalf("断流盲重试未生效（终态应是续写段的 stop）:\n%s", s)
	}
}
