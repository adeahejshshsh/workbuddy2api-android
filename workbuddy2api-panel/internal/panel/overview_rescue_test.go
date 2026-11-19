package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 今日救援次数必须一路到 overview（issue #273 第二条评论：只有网关日志里能看见，
// 面板上看不出来，异常触发时「今天怎么突然贵了」没有出口）。
func TestOverviewCarriesRescueToday(t *testing.T) {
	pn := New(Config{
		Version:     "test",
		Pool:        pool.New(""),
		RescueToday: func() map[string]int { return map[string]int{"narration": 3, "toolonly": 1} },
	})
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	var d struct {
		RescueToday map[string]int `json:"rescue_today"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.RescueToday["narration"] != 3 || d.RescueToday["toolonly"] != 1 {
		t.Fatalf("overview 丢了 rescue_today: %s", rec.Body)
	}
}
