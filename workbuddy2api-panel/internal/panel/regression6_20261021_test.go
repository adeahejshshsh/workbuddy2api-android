package panel

import (
	"strings"
	"testing"
)

// BUG-2：app.js 里用到的 inReqRange 必须有定义。
//
// v1.29.0 曾在 loadLogs 里直接调它却忘了定义 —— 归档关闭时整页抛 ReferenceError，
// 运行日志固定显示「读取失败」。这条用例专门钉住「调用了但没定义」这种低级但
// 整页失效的错误（同类问题此前还有 reqErr / mdQTimer）。
func TestAppJSInReqRangeDefined(t *testing.T) {
	src := string(appJS)
	if !strings.Contains(src, "function inReqRange(") {
		t.Fatal("app.js 缺少 function inReqRange 定义（调用点会抛 ReferenceError）")
	}
	// 有调用点才算数（避免定义了却没人用、或反过来）
	if strings.Count(src, "inReqRange(") < 2 {
		t.Fatal("inReqRange 只有定义没有调用（或调用被误删）")
	}
}
