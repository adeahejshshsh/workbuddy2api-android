package server

// W2-04 回归：换段 goroutine 的 panic 必须被 recover 成 segOpenResult。
//
// 这个 goroutine 不是 net/http 起的，Go 不会替它 recover —— 一次 panic 就是
// 进程级 fatal error，所有在途 SSE 一起断。测试用 nil upstream 触发真实 panic
// （nil *upstream.Client 解引用），断言它变成一条错误结果而不是带走测试进程。

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestOpenSegmentAsyncRecoversPanic(t *testing.T) {
	r := &continueReader{openCtx: context.Background()}
	started := make(chan struct{})
	ok := r.openSegmentAsync(0, []byte(`{}`), func(io.ReadCloser) {
		t.Error("panic 不应走 onOK")
	}, func(int, error) {
		close(started)
	})
	if !ok {
		t.Fatal("openSegmentAsync 应接受本次请求")
	}
	select {
	case res := <-r.pending.ch:
		if res.err == nil {
			t.Fatal("panic 应折成 error 结果")
		}
		if !strings.Contains(res.err.Error(), "panic") {
			t.Fatalf("错误应说明是 panic：%v", res.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("panic 后没有回传结果（goroutine 挂了或卡死）")
	}
}
