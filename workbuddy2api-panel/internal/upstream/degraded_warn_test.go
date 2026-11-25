package upstream

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// issue #283：永续降级（企业端点 500、/v3 接住）每轮刷新都重打一遍 WARN，4 天 49 次。
// 这里锁死「同 key 只报一次、不同 key 各报一次」。
func TestDegradedWarnOnceThrottles(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	degradedWarnOnce("t-once", "WARN: boom %d", 1)
	degradedWarnOnce("t-once", "WARN: boom %d", 1)
	degradedWarnOnce("t-once", "WARN: boom %d", 1)
	degradedWarnOnce("t-other", "WARN: other %d", 2)

	out := buf.String()
	if n := strings.Count(out, "boom"); n != 1 {
		t.Errorf("同 key 只该报一次，实际 %d 次：%s", n, out)
	}
	if n := strings.Count(out, "other"); n != 1 {
		t.Errorf("不同 key 各报一次，实际 %d 次：%s", n, out)
	}
}
