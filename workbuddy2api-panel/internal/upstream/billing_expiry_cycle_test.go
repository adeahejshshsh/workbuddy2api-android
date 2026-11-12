// 国际版到期批次的**按周期**聚合回归（上游 issue #200）。
//
// 现场：summary 报 remain=1027，明细 32 行**同一个 PackageCode**（每天发一个 Bonus
// Pack，各自 CycleEndTime 逐日递推、各带自己的 CycleCapacityRemain）。旧代码按码归并
// 只留「最早的未来到期」那条，再把 summary 里当前周期的剩余配上去，于是面板显示
// 「2026-11-11（明天到期）remain=927」；更严重的是 expiring 也被整笔算进去，池子会把
// 该号当成「积分即将作废」优先消耗。
//
// 本仓没有上游的 ExpiryBucket 结构，所以这里断言我们实际消费的五个返回值：
// remain / total / expiring / earliestAt / earliestRemaining。
package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type expiryRow struct {
	code    string
	end     time.Time
	remain  float64
	total   float64
	summary *float64 // 非 nil：该码在 summary 里的「当前周期剩余」
}

func expiryAggregate(t *testing.T, rows []expiryRow, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64) {
	t.Helper()
	type sumRow struct {
		code   string
		total  float64
		remain float64
	}
	var sums []sumRow
	idx := map[string]int{}
	for _, r := range rows {
		i, ok := idx[r.code]
		if !ok {
			i = len(sums)
			idx[r.code] = i
			sums = append(sums, sumRow{code: r.code})
		}
		sums[i].total += r.total
		sums[i].remain += r.remain
	}
	for _, r := range rows {
		if r.summary != nil {
			sums[idx[r.code]].remain = *r.summary
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
			var parts []string
			for _, s := range sums {
				parts = append(parts, fmt.Sprintf(
					`{"PackageCode":%q,"CycleTotalCapacity":"%.2f","CycleRemainCapacity":"%.2f"}`,
					s.code, s.total, s.remain))
			}
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Packages":[` + strings.Join(parts, ",") + `]}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-free-packages"):
			var parts []string
			for _, r2 := range rows {
				parts = append(parts, fmt.Sprintf(
					`{"PackageCode":%q,"PackageName":"Bonus Pack","Status":1,`+
						`"CycleCapacitySize":%.2f,"CycleCapacityRemain":%.2f,"CycleCapacityRemainPrecise":%.2f,`+
						`"CycleEndTime":%q,"DeductionEndTime":%d}`,
					r2.code, r2.total, r2.remain, r2.remain,
					r2.end.In(softRateResetLoc).Format("2006-01-02 15:04:05"), r2.end.UnixMilli()))
			}
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[` + strings.Join(parts, ",") + `],"TotalCount":` +
				fmt.Sprint(len(rows)) + `}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-paid-packages"):
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[],"TotalCount":0}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	remain, total, expiring, earliestAt, earliestRemaining, err := c.globalUserResource(globalAuth(), soon)
	if err != nil {
		t.Fatalf("globalUserResource: %v", err)
	}
	return remain, total, expiring, earliestAt, earliestRemaining
}

func dayAt(days int) time.Time {
	base := time.Now().In(softRateResetLoc)
	// 取当日 00:00 而不是 12:00（上游 d011fc8）：到期窗口是「now + N 小时」判的，
	// 固定 12:00 会让「第 N 天算不算在窗口内」随时钟漂移（上午跑少算一天、下午跑才对）。
	// 00:00 对任何时刻都稳定落在窗口内，D+8 则稳定落在窗口外。
	return time.Date(base.Year(), base.Month(), base.Day()+days, 0, 0, 0, 0, softRateResetLoc)
}

func f64p(v float64) *float64 { return &v }

// 同码三条明细（一条已用完的旧周期 + 两条还有余额的新周期）：
// 旧实现会把 summary 的 59 全挂到最早的 11-11 上，并把 expiring 也算成 59。
func TestGlobalExpiryKeepsCyclesSeparate(t *testing.T) {
	rows := []expiryRow{
		{code: "PKG-BONUS", end: dayAt(1), remain: 0, total: 30},   // 旧周期：明天到期但已用完
		{code: "PKG-BONUS", end: dayAt(29), remain: 29, total: 30}, // 两个仍有效的周期，各带自己的剩余
		{code: "PKG-BONUS", end: dayAt(30), remain: 30, total: 30},
	}
	_, _, expiring, earliestAt, earliestRem := expiryAggregate(t, rows, 0)

	if got, want := earliestAt.Format("2006-01-02"), dayAt(29).Format("2006-01-02"); got != want {
		t.Fatalf("earliestAt=%s want %s（已用完的旧周期不得拉早最早到期日）", got, want)
	}
	if earliestRem != 29 {
		t.Fatalf("earliestRemaining=%d want 29（用该周期自己的剩余，不借 summary）", earliestRem)
	}
	if expiring != 0 {
		t.Fatalf("expiring=%d want 0（快过期窗口关闭，且旧周期已用完）", expiring)
	}
}

// 快过期窗口打开时，只有落在窗口内的**自己那份**余额计入 expiring。
func TestGlobalExpiringUsesOwnRemain(t *testing.T) {
	rows := []expiryRow{
		{code: "PKG-BONUS", end: dayAt(1), remain: 27, total: 30},
		{code: "PKG-BONUS", end: dayAt(3), remain: 30, total: 30},
		{code: "PKG-BONUS", end: dayAt(30), remain: 30, total: 30},
	}
	// 窗口锚到日界而不是固定 168h（上游 cf2b80f）：D+7 的 00:00 是否落在「now+168h」内
	// 取决于此刻是 CST 的哪个时段，固定窗口会让边界断言每天 00:00–12:00 必红。
	_, _, expiring, _, _ := expiryAggregate(t, rows, dayAt(7).Sub(time.Now()))
	if expiring != 57 {
		t.Fatalf("expiring=%d want 57（27 + 30，不能把 30 天那批也算进来）", expiring)
	}
}

// 单周期码且明细自报 0、summary 有数 → 允许借 summary（套餐型包的历史行为）。
func TestGlobalSingleCycleBorrowsSummary(t *testing.T) {
	rows := []expiryRow{
		{code: "PKG-PLAN", end: dayAt(20), remain: 0, total: 500, summary: f64p(500)},
	}
	_, _, _, _, earliestRem := expiryAggregate(t, rows, 0)
	if earliestRem != 500 {
		t.Fatalf("earliestRemaining=%d want 500（单周期可借 summary 余额）", earliestRem)
	}
}

// 多周期码不允许借 summary：否则新周期余额会被摊到每个周期上（issue #200 的根因）。
func TestGlobalMultiCycleDoesNotBorrowSummary(t *testing.T) {
	rows := []expiryRow{
		{code: "PKG-BONUS", end: dayAt(10), remain: 0, total: 30},
		{code: "PKG-BONUS", end: dayAt(11), remain: 30, total: 30},
	}
	_, _, _, earliestAt, earliestRem := expiryAggregate(t, rows, 0)
	// 第一条自报 0 且码有多个周期 → 跳过；最早到期落在第 11 天那批。
	if got, want := earliestAt.Format("2006-01-02"), dayAt(11).Format("2006-01-02"); got != want {
		t.Fatalf("earliestAt=%s want %s", got, want)
	}
	if earliestRem != 30 {
		t.Fatalf("earliestRemaining=%d want 30（不得借 summary 的 60）", earliestRem)
	}
}
