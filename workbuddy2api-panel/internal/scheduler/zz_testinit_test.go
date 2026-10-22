package scheduler

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 测试初始化：把「账号间节流」与「计费重试退避」全部关到最小。
//
// 这些都是**生产用的等待**，单测里真睡会把整包从毫秒级拖到分钟级：
//   - 账号间节流（800ms × 账号数）：曾把 checkin/activity/travel 用例拖到十几秒；
//   - 计费重试退避（1×base + 2×base）：一个「上游全 500」的用例因此固定睡 12s。
//
// 注意 accountDelay 的 fallback 是**按任务传入**的（activity/travel 各自一个变量），
// 所以三个都要置 0 —— 只置一个会让另外两条链路继续真睡。
func init() {
	keepaliveAccountDelay = 0
	activityAccountDelay = 0
	travelAccountDelay = 0
	checkinAccountDelay = 0
	upstream.SetBillingRetryDelayForTest(time.Millisecond)
}
