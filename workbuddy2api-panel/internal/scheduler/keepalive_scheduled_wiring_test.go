package scheduler

import (
	"os"
	"strings"
	"testing"
)

// 排程路径必须走 selective 保活（审查 E5）：此前 runBatch 调的是公开入口
// RunKeepaliveNowCtx(ctx, true)，而它固定 selective=false —— keepalive_refresh_window
// 在生产路径完全没生效，每天 22:00 仍给全池每个号换一次 RT。
//
// 这条用例读源码而不是跑行为：行为级用例（keepalive_window_test）自造
// runKeepalive(...,true) 调用，正是它把「假绿」固化下来的 —— 只要 runBatch 的接线
// 又改回公开入口，那条用例照样绿。这里直接钉住接线本身。
func TestRunBatchUsesSelectiveKeepalive(t *testing.T) {
	raw, err := os.ReadFile("scheduler.go")
	if err != nil {
		t.Fatalf("读 scheduler.go: %v", err)
	}
	src := string(raw)
	// `case taskKeepalive:` 在源码里出现不止一次（族名映射那个 switch 也有一处），
	// 所以逐处检查、取**真正调用保活**的那一段。
	found := false
	for off := 0; ; {
		i := strings.Index(src[off:], "case taskKeepalive:")
		if i < 0 {
			break
		}
		start := off + i
		rest := src[start:]
		// 片段截到下一条 case（族名映射那段里没有 runKeepalive，会被下面的 continue 跳过）。
		if j := strings.Index(rest, "case taskGrowth:"); j > 0 {
			rest = rest[:j]
		} else if j := strings.Index(rest, "case task"); j > 0 && j != 0 {
			rest = rest[:j]
		}
		off = start + len("case taskKeepalive:")
		if !strings.Contains(rest, "runKeepalive") {
			continue // 不是排程分支（族名映射之类）
		}
		found = true
		// 只看**代码行**：注释里会提到被替换掉的旧调用（说明为什么改），不能算命中。
		var code []string
		for _, ln := range strings.Split(rest, "\n") {
			if t := strings.TrimSpace(ln); strings.HasPrefix(t, "//") {
				continue
			}
			code = append(code, ln)
		}
		rest = strings.Join(code, "\n")
		if strings.Contains(rest, "RunKeepaliveNowCtx") {
			t.Fatalf("排程保活又调回全量入口 RunKeepaliveNowCtx，keepalive_refresh_window 会重新失效：\n%s", rest)
		}
		if !strings.Contains(rest, "runKeepalive(ctx, true, true)") {
			t.Fatalf("排程保活应调 runKeepalive(ctx, true, true)（selective=true）：\n%s", rest)
		}
	}
	if !found {
		t.Fatal("找不到 runBatch 里调用保活的 taskKeepalive 分支")
	}
}
