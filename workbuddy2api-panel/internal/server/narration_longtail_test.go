// narration_longtail_test.go 无标点长正文只救预告下一步动作的末句。
package server

import (
	"strings"
	"testing"
)

// 写完的长总结最后一行常常没有句号（列表项、表格行、「全部测试通过 ✅」、文件路径），
// 放开长度闸门后它们会被续写一段（还被告知「承诺的动作没执行」），每次都是整段上下文
// 重发的计费请求。
func TestNarrationLongFinishedTailNotRescued(t *testing.T) {
	long := strings.Repeat("这一轮我把三处改动逐条过了一遍，结论如下。", 30) + "\n\n"
	for _, tail := range []string{
		"## 改动清单\n- 修复续写闸门\n- 新增回归测试",
		"| 文件 | 改动 |\n|---|---|\n| continue.go | 续写闸门 |",
		"全部测试通过 ✅",
		"任务已全部完成",
		"改动见 internal/server/continue.go",
		"接下来你可以运行 go test ./... 验证",
		"对我来说这次改动已经足够",
		"Testing: all 42 cases passed",
	} {
		text := long + tail
		if !narrationTailSuspends(text) || len(text) <= narrationMaxBytes {
			t.Fatalf("前提失败：%q 应是超长的无标点收尾", tail)
		}
		if n := narrationRescueCount(t, text); n != 0 {
			t.Errorf("长总结以 %q 收尾不该续写，实际 %d 次", tail, n)
		}
	}
}

// 反方向：长正文停在各种说法的预告句上（#245 的形态）都要救——只认「我将 / 我会」会把
// 「我需要先读取…」「Let's fix…」「正在修改…」这类同样被掐断的句子漏掉。
func TestNarrationLongPromiseTailStillRescued(t *testing.T) {
	long := strings.Repeat("这一轮我把三处改动逐条过了一遍，结论如下。", 30) + "\n\n"
	for _, tail := range []string{
		"我需要先读取 internal/server/foo.go",
		"我打算先改续写闸门",
		"我开始修改 continue.go",
		"接下来开始修改 narration.go",
		"现在开始修改测试",
		"下一步我再补回归用例",
		"我们接着改 toolonly.go",
		"正在修改 foo.go",
		"Now I need to update the gate",
		"I need to update the tests",
		"Let's fix the gate first",
		"Reading failfirst.sh mutation script",
	} {
		if n := narrationRescueCount(t, long+tail); n != 1 {
			t.Errorf("长正文停在预告句 %q 上应救一次，实际 %d 次", tail, n)
		}
	}
}
