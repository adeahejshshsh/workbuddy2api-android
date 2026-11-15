package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 别人的 job_id 不能查（审查 A6）：台账记提交方指纹，非本调用方一律按「不存在」处理，
// 否则拿到（或猜到）task_id 就等于拿到带 q-signature 的产物直链。
func TestThreeDTaskScopedToSubmitter(t *testing.T) {
	h := NewHandler(Config{APIKey: "k1", ThreeDEnabled: true, Upstream: upstream.New()})
	h.threeDLedger.putFor("job-1", "u1", "cn", "3.1",
		h.callerPrint(reqWithKey("Bearer k1")),
		upstream.ThreeDState{JobID: "job-1", Status: "DONE", ResultURL: "https://x.myqcloud.com/a.glb"})
	if !h.threeDLedger.ownedBy("job-1", h.callerPrint(reqWithKey("Bearer k1"))) {
		t.Fatal("提交方自己应该能查到")
	}
	if h.threeDLedger.ownedBy("job-1", h.callerPrint(reqWithKey("Bearer k2"))) {
		t.Fatal("别的 key 不该能查到别人的任务（会泄漏产物直链）")
	}
	// 未配 key（无租户边界）：不做校验，保持既有行为。
	h2 := NewHandler(Config{ThreeDEnabled: true, Upstream: upstream.New()})
	h2.threeDLedger.putFor("job-2", "u1", "cn", "3.1", "", upstream.ThreeDState{JobID: "job-2"})
	if !h2.threeDLedger.ownedBy("job-2", "whatever") {
		t.Fatal("未配 key 时不该做归属校验")
	}
}

// 视频台账同一条规则。
func TestVideoTaskScopedToSubmitter(t *testing.T) {
	h := NewHandler(Config{APIKey: "k1", Upstream: upstream.New()})
	h.videoLedger.putFor("task-1", "u1", "cn", "minimax", h.callerPrint(reqWithKey("Bearer k1")))
	if !h.videoLedger.ownedBy("task-1", h.callerPrint(reqWithKey("Bearer k1"))) {
		t.Fatal("提交方自己应该能查到")
	}
	if h.videoLedger.ownedBy("task-1", h.callerPrint(reqWithKey("Bearer k2"))) {
		t.Fatal("别的 key 不该能查到别人的视频任务")
	}
	// 旧条目（没有 Caller）视为公共，兼容进程重启前的台账。
	h.videoLedger.put("task-2", "u1", "cn", "minimax")
	if !h.videoLedger.ownedBy("task-2", "anyone") {
		t.Fatal("没有归属记录的旧条目应保持可查")
	}
	_ = time.Now()
}

func reqWithKey(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/3d/x", nil)
	r.Header.Set("Authorization", authz)
	return r
}
