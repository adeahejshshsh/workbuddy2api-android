// billing_summary.go 国际版（global realm）计费新口径：summary + paid/free packages。
//
// 为什么单独一条链：官方 5.8.4 国际版的额度查询改成了三件套（真机抓包实测，
// 2026-10-20，www.workbuddy.ai）：
//
//	POST /billing/meter/get-user-resource-summary      {}   ← 当前周期聚合（remain/total）
//	POST /billing/meter/get-user-resource-paid-packages {}  ← 付费包明细（带到期时间）
//	POST /billing/meter/get-user-resource-free-packages {}  ← 免费包明细（带到期时间）
//
// 而本仓原先走的是 CN 那套 `/billing/meter/get-user-resource` 带分页 body
// （PageNumber/PageSize/ProductCode/Status/PackageEndTimeRange*）。国际版对该路径
// 返回的形态已与 CN 不同（summary 里 CycleTotalCapacity / CycleRemainCapacity 是
// **字符串小数**，且没有 Accounts 树），继续按 CN 解析会得到恒 0 的余额——积分保底
// 随即把这些号当耗尽号挡出选号。
//
// 分工：summary 给权威的 remain/total（当前周期口径），paid/free 明细只用来补齐
// 「哪天到期、那一批还剩多少」。两者按 PackageCode 关联——summary 的 Packages 也带
// PackageCode，故能精确对齐，不必靠顺序或名字猜。明细拉不到时降级为「只有总额没有
// 到期批次」（面板少一行到期提醒），绝不因为明细失败把总额也丢掉。
//
// 与上游的差异：上游另有一份「按自然日聚合的到期桶」（ExpiryBucket）供面板首页快照；
// 本仓面板直接从逐包的 ExpiresAt 现算（app.js 的 summarizeCreditDays），故这里只产
// 逐包明细，不重复造一份桶。
package upstream

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

const (
	billingSummaryPath = "/billing/meter/get-user-resource-summary"
	billingPaidPath    = "/billing/meter/get-user-resource-paid-packages"
	billingFreePath    = "/billing/meter/get-user-resource-free-packages"
)

// flexFloat 解析「可能是 JSON 数字也可能是字符串」的容量字段。
//
// 上游同一语义字段两套形态并存：summary 里是字符串小数（"335.47000026"），
// 明细里是 JSON 整数 + 独立的 *Precise 字符串。用这个类型兜住，避免为每个字段
// 写两遍解析，也避免字符串形态被 json.Unmarshal 直接判错整段失败。
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "null" || s == `""` {
		*f = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// 单个脏字段不该让整次余额查询失败：按 0 处理（该项计不进去，其余照算）。
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

// billingInner 兼容两种信封层级。
//
// doJSON 已解掉最外层的 apiEnvelope（code/msg/data），所以拿到的 raw 是 data 的内容。
// 新三件套（summary / paid / free）的 data 直接就是 {"Packages":…} / {"Accounts":…}；
// 但 CN 老端点与部分域仍会再包一层 {"Response":{"Data":{…}}}（CreditPackages 里那条
// 「不能再套一层 Code/Data」的注释说的就是这件事）。两种都认：先看顶层有没有目标键，
// 没有就下钻 Response.Data。写死一种层级的话，另一种形态会静默解析成空结构——
// 表现为「余额恒 0」，正是本文件要修的那个症状。
func billingInner(raw json.RawMessage, key string) json.RawMessage {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return raw
	}
	if _, ok := probe[key]; ok {
		return raw
	}
	var wrapped struct {
		Response struct {
			Data json.RawMessage `json:"Data"`
		} `json:"Response"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Response.Data) > 0 {
		return wrapped.Response.Data
	}
	return raw
}

// summaryPackage summary.Packages[] 的单条：当前周期的用量聚合。
type summaryPackage struct {
	PackageCode         string    `json:"PackageCode"`
	CycleTotalCapacity  flexFloat `json:"CycleTotalCapacity"`
	CycleRemainCapacity flexFloat `json:"CycleRemainCapacity"`
	CycleUsedCapacity   flexFloat `json:"CycleUsedCapacity"`
	CapacityUnit        string    `json:"CapacityUnit"`
}

// packageDetail paid/free packages 的单条：与 CN 的 Accounts[] 同构，另带 *Precise
// 字符串形态，以及用于判定到期的三个时间字段。
type packageDetail struct {
	PackageCode         string    `json:"PackageCode"`
	PackageName         string    `json:"PackageName"`
	DeductionEndTime    int64     `json:"DeductionEndTime"` // epoch 毫秒，真实扣费截止
	ExpiredTime         string    `json:"ExpiredTime"`
	CycleEndTime        string    `json:"CycleEndTime"`
	CapacitySize        flexFloat `json:"CapacitySize"`
	CapacityRemain      flexFloat `json:"CapacityRemain"`
	CapacityUsed        flexFloat `json:"CapacityUsed"`
	CycleCapacitySize   flexFloat `json:"CycleCapacitySize"`
	CycleCapacityRemain flexFloat `json:"CycleCapacityRemain"`
	CycleCapacityUsed   flexFloat `json:"CycleCapacityUsed"`
	// *Precise 优先：整数形态会把 0.47 这类小数截断，两者都在时以 Precise 为准。
	CapacitySizePrecise        flexFloat `json:"CapacitySizePrecise"`
	CapacityRemainPrecise      flexFloat `json:"CapacityRemainPrecise"`
	CycleCapacitySizePrecise   flexFloat `json:"CycleCapacitySizePrecise"`
	CycleCapacityRemainPrecise flexFloat `json:"CycleCapacityRemainPrecise"`
}

// summaryReply / packagesReply 是两条端点的信封内层。
type summaryReply struct {
	Packages                []summaryPackage `json:"Packages"`
	SubscriptionPackageName string           `json:"SubscriptionPackageName"`
	IsPaidUser              *bool            `json:"IsPaidUser"` // nil = 报文里没有：不知道，不等于免费
}

type packagesReply struct {
	Accounts   []packageDetail `json:"Accounts"`
	TotalCount int             `json:"TotalCount"`
}

// globalUserResource 国际版额度查询：summary 给总额，paid/free 明细补到期批次。
// 返回值口径与 CN 路径完全一致（remain/total/expiring/earliest/earliestRemaining），
// 上游调用方无感知。
// ExpiryBucket 单个自然日到期的积分合计（按真实失效时刻聚合）。
//
//   - Date 为该日（UTC+8，YYYY-MM-DD），前端按这个日期分组展示；
//   - ExpireAt 是该日最早一批的精确时刻；
//   - Remain 是该日所有正余额包的剩余量之和；
//   - Estimated 该日额度里至少有一批的到期日是**按计量周期（CycleEndTime）推算**的，
//     不是上游给出的真实扣费截止（缺 DeductionEndTime / ExpiredTime 时的回退）。面板据此
//     标注「按计量周期推算」，避免运维把推算值当权威日期（issue #200）。
type ExpiryBucket struct {
	Date      string    `json:"date"`
	ExpireAt  time.Time `json:"expires_at,omitempty"`
	Remain    int64     `json:"remain"`
	Estimated bool      `json:"estimated,omitempty"`
}

// estimatedExpiryWarnEvery 同一账号「到期日只能推算」WARN 的最小间隔。余额刷新每 5 分钟
// 一轮，而这个状态由上游数据形态决定、短时间内不会变；不限频会让每个号每天刷 288 行
// 同样的日志。变量供测试缩短。
var estimatedExpiryWarnEvery = time.Hour

// estimatedExpiryWarnLimit 限频表的条目上限（审查 W1-13）。
//
// 此前是包级 sync.Map 且**只写不删**：uid 来自导入账号，删号/换号后条目永久留在
// 进程里；网关 7×24 常驻、账号反复导入导出，这张表就是单调增长。sync.Map 没有
// Len()，连「现在多大」都观测不到，所以改成带清理的 mutex map。
// 4096 个条目足够覆盖任何真实账号池（超出的部署应按分片/多实例部署，而不是靠这张表）。
// 变量供测试缩短规模（与 estimatedExpiryWarnEvery 同一约定）。
var estimatedExpiryWarnLimit = 4096

var (
	estimatedExpiryWarnMu sync.Mutex
	// estimatedExpiryWarned uid -> 上次 WARN 时刻。
	estimatedExpiryWarned = make(map[string]time.Time)
)

// pruneEstimatedExpiryWarnedLocked 清掉「已经超过 2 个间隔没再出现」的 uid。
//
// 为什么是 2 个间隔：条目本身只影响「同一间隔内不重复写日志」，清掉一个仍在
// 刷新的 uid 不会改变语义（下次调用直接重新写一条 WARN），所以清理是安全的；
// 但正常账号每 5 分钟刷一轮、间隔 1 小时，留 2 倍余量避免把活跃账号在临界点上反复清。
// 清完仍超上限（全都活跃，说明池子规模本身超过常量）时淘汰最老的一条，保证有界。
// 调用方必须持有 estimatedExpiryWarnMu。
func pruneEstimatedExpiryWarnedLocked(now time.Time) {
	if len(estimatedExpiryWarned) < estimatedExpiryWarnLimit {
		return
	}
	keepAfter := estimatedExpiryWarnEvery * 2
	for uid, last := range estimatedExpiryWarned {
		if now.Sub(last) >= keepAfter {
			delete(estimatedExpiryWarned, uid)
		}
	}
	if len(estimatedExpiryWarned) < estimatedExpiryWarnLimit {
		return
	}
	oldestUID := ""
	var oldest time.Time
	for uid, last := range estimatedExpiryWarned {
		if oldestUID == "" || last.Before(oldest) {
			oldestUID, oldest = uid, last
		}
	}
	if oldestUID != "" {
		delete(estimatedExpiryWarned, oldestUID)
	}
}

// estimatedExpiryWarnDue 返回该账号这次是否该写 WARN，并记下时刻。
//
// 多个刷新协程可能同时算同一账号：整段在锁内判定 + 记录，保证同一间隔内只有
// 一个调用方拿到 true（与升级前的 LoadOrStore+CAS 语义一致）。
func estimatedExpiryWarnDue(uid string, now time.Time) bool {
	if uid == "" {
		return false
	}
	estimatedExpiryWarnMu.Lock()
	defer estimatedExpiryWarnMu.Unlock()
	if last, ok := estimatedExpiryWarned[uid]; ok && now.Sub(last) < estimatedExpiryWarnEvery {
		return false
	}
	pruneEstimatedExpiryWarnedLocked(now)
	estimatedExpiryWarned[uid] = now
	return true
}

// globalUserResourceBuckets 同 globalUserResource，另外给出「按自然日聚合的到期桶」
// （面板首页与到期提醒用）。两个入口共用同一份实现，口径不会漂移。
func (c *Client) globalUserResourceBuckets(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, buckets []ExpiryBucket, err error) {
	// summary 是主口径，失败即整次失败（明细单独拿到也没总额可对）。
	raw, err := c.billingJSON(a, "POST", billingSummaryPath, map[string]any{})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, nil, err
	}
	var sum summaryReply
	if uerr := json.Unmarshal(billingInner(raw, "Packages"), &sum); uerr != nil {
		return 0, 0, 0, time.Time{}, 0, nil, fmt.Errorf("billing summary parse: %w", uerr)
	}
	// 顺手记下这个号是不是付费号：选号对 exclusive 模型（免费号调不通）据此跳过它。
	// 零额外请求——这个字段本来就在 summary 里，此前只解析不用。
	if a != nil && sum.IsPaidUser != nil {
		c.NotePaidUser(a.UID, *sum.IsPaidUser)
	}
	now := time.Now()
	// summary 的 Packages 是「当前周期的权威用量」：remain/total 以它为准。
	remainByCode := make(map[string]int64, len(sum.Packages))
	codes := make([]string, 0, len(sum.Packages))
	for _, p := range sum.Packages {
		// 四舍五入而不是截断（审查 A1-07）：CycleRemainCapacity 是字符串小数
		// （"335.47000026"），直接 int64() 会把 0.9 分余额记成 0 —— 面板显示「余额 0」，
		// 路由权重与 credit_floor 也跟着低估。口径与 CN 侧 packageRemainUsed 一致。
		r := roundCapacity(float64(p.CycleRemainCapacity))
		s := roundCapacity(float64(p.CycleTotalCapacity))
		if r < 0 {
			r = 0
		}
		if r > s && s > 0 {
			r = s
		}
		remain += r
		total += s
		if p.PackageCode != "" {
			remainByCode[p.PackageCode] = r
			codes = append(codes, p.PackageCode)
		}
	}

	// 明细只为「到期批次」服务：失败不影响总额（降级为无到期提醒）。
	details := c.globalPackageDetails(a, codes)
	// 同一 PackageCode 在明细里会出现**多条**（每个计量周期一条：ResourceCycleId 不同、
	// CycleEndTime 逐月递推）。按码归并——每个包只该贡献一次余额，且只认该码**最早
	// 的未来到期**那条（真正会先扣完的那一期）。不归并会把同一笔余额重复计入 N 次。
	type pick struct {
		d         packageDetail
		end       time.Time
		estimated bool // 到期日是按计量周期推算的
	}
	// 归并的最小单位是 **（PackageCode, 计量周期）**，不是 PackageCode 本身。
	//
	// 错在哪（上游 issue #200）：国际版 Bonus Pack 每天发一个包、**所有包共用同一个
	// PackageCode**，明细里同码多条、CycleEndTime 逐日递推。此前只按码归并、取「最早
	// 的未来到期」那条，再把 summary 里**当前周期**的剩余配到这个旧 end 上：
	//
	//	旧周期（已用完）的 end + 新周期的 remain → 面板显示「927 分明天到期」，
	//	真实情况是「明天 27 分、后天 30 分……逐日到期」。
	//
	// 更坏的是 expiring（7 天内快过期额度）也被整笔算进去，池子会把这号当成
	// 「积分马上作废」优先消耗——不只是显示问题。
	type cycleKey struct {
		code string
		end  int64 // Unix 秒：同一周期同码的 end 必然相同，用它当周期标识
	}
	merged := make(map[cycleKey]pick, len(details))
	for _, d := range details {
		end, ok, estimated := packageEndAt(d.DeductionEndTime, d.ExpiredTime, d.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		k := cycleKey{d.PackageCode, end.Unix()}
		if cur, seen := merged[k]; seen {
			// 同一批可能同时出现在 paid / free 两条端点：留剩余更大的一条。
			if d.balanceRemain() > cur.d.balanceRemain() {
				merged[k] = pick{d: d, end: end, estimated: estimated}
			}
			continue
		}
		merged[k] = pick{d: d, end: end, estimated: estimated}
	}
	// 每个码有多少个未来周期：只有一个时「借 summary 余额」不会跨周期串味（历史行为，
	// 覆盖「明细自报 0 而 summary 有数」的套餐型包）；多个时一律不借。
	futurePerCode := make(map[string]int, len(merged))
	for k := range merged {
		futurePerCode[k.code]++
	}
	byDate := make(map[string]*ExpiryBucket)
	for k, p := range merged {
		// 每行用**自己的**剩余；只有单周期码才允许回落到 summary 的余额。
		// 判「是否用完」必须用 **float 原值**（审查 A1-07）：balanceRemain 是字符串
		// 小数（"0.9" 这种），先 int64() 截断再判 `<= 0` 会把 0.9 分当成「已用完」，
		// 该批到期信息整条丢弃 —— earliestAt/expiring 少算，池子可能把仍有余额的号
		// 判成耗尽。四舍五入只发生在**计入金额**那一步。
		rf := p.d.balanceRemain()
		if rf <= 0 && futurePerCode[k.code] == 1 {
			if sr, ok := remainByCode[k.code]; ok {
				rf = float64(sr)
			}
		}
		if rf <= 0 {
			// 该周期已用完 / 明细没给剩余：不贡献额度，也不该拉早 earliestAt。
			continue
		}
		r := roundCapacity(rf)
		end := p.end
		if earliestAt.IsZero() || end.Before(earliestAt) {
			earliestAt = end
		}
		day := end.In(softRateResetLoc).Format("2006-01-02")
		b, seen := byDate[day]
		if !seen {
			b = &ExpiryBucket{Date: day, ExpireAt: end}
			byDate[day] = b
		} else if end.Before(b.ExpireAt) {
			b.ExpireAt = end
		}
		if p.estimated {
			// 同日多批里只要有一批是推算的，整行就标注——面板一行只展示「该日」，
			// 混了两种口径时保守地显示为推算。
			b.Estimated = true
		}
		b.Remain += r
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring += r
		}
	}
	var estimateCount int
	if len(byDate) > 0 {
		dates := make([]string, 0, len(byDate))
		for d := range byDate {
			dates = append(dates, d)
		}
		sort.Strings(dates)
		buckets = make([]ExpiryBucket, 0, len(dates))
		for _, d := range dates {
			buckets = append(buckets, *byDate[d])
			if byDate[d].Estimated {
				estimateCount++
			}
		}
		earliestRemaining = buckets[0].Remain
	}
	if estimateCount > 0 && a != nil && estimatedExpiryWarnDue(a.UID, now) {
		// 日期是按计量周期末日推算的（缺 DeductionEndTime / ExpiredTime）。仍然计入，
		// 但留一行 WARN：这个口径不对等于真实扣费截止，排障时别被它带偏。
		log.Printf("WARN: [billing] %d 个自然日的到期日只能按计量周期（CycleEndTime）推算，非真实扣费截止（acct=%s）",
			estimateCount, a.UID)
	}
	// 与 CN 口径统一（审查 A1-03）：summary 回 200 但 Packages 为空时 remain=0 是
	// **没有观测**，不是「余额归零」。此前无条件 notifyCredits，积分台账里就多出
	// 一条假「余额归零」观测，下一轮真实余额再产生一条虚假大额 delta，下游若据台账
	// 做余额判断会误杀好号。CN 侧早有 hasAccounts 守卫（client.go），这里补齐。
	if len(sum.Packages) > 0 {
		c.notifyCredits(a, remain, nil)
	}
	return remain, total, expiring, earliestAt, earliestRemaining, buckets, nil
}

// globalUserResource 兼容旧调用方：只关心余额与最早到期，不要桶。
func (c *Client) globalUserResource(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	remain, total, expiring, earliestAt, earliestRemaining, _, err = c.globalUserResourceBuckets(a, soon)
	return
}

// globalCreditPackages 国际版的逐包构成（面板「积分构成」用），口径与 CN 的
// CreditPackages 对齐：remain/size 由 summary 给，逐包到期由明细补。
//
// 与 globalUserResource 同源：summary 决定「有哪些包、各剩多少」，明细只决定
// 「这个包哪天到期」。明细缺失时仍返回逐包余额，只是没有 EndTime。
func (c *Client) globalCreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	raw, err := c.billingJSON(a, "POST", billingSummaryPath, map[string]any{})
	if err != nil {
		return nil, 0, 0, err
	}
	var sum summaryReply
	if uerr := json.Unmarshal(billingInner(raw, "Packages"), &sum); uerr != nil {
		return nil, 0, 0, fmt.Errorf("billing summary parse: %w", uerr)
	}
	now := time.Now()
	codes := make([]string, 0, len(sum.Packages))
	for _, p := range sum.Packages {
		if p.PackageCode != "" {
			codes = append(codes, p.PackageCode)
		}
	}
	// 每个码取「最早未来到期」那条，用于补 EndTime / PackageName。
	type pick struct {
		d         packageDetail
		end       time.Time
		estimated bool // 到期日是按计量周期推算的
	}
	best := make(map[string]pick, len(codes))
	for _, d := range c.globalPackageDetails(a, codes) {
		end, ok, _ := packageEndAt(d.DeductionEndTime, d.ExpiredTime, d.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		cur, seen := best[d.PackageCode]
		if !seen || end.Before(cur.end) {
			best[d.PackageCode] = pick{d: d, end: end}
		}
	}
	out := make([]CreditPackage, 0, len(sum.Packages))
	var remainSum, totalSum int64
	for _, p := range sum.Packages {
		// 同上：字符串小数四舍五入（审查 A1-07）。
		remain := roundCapacity(float64(p.CycleRemainCapacity))
		size := roundCapacity(float64(p.CycleTotalCapacity))
		if remain < 0 {
			remain = 0
		}
		if remain > size && size > 0 {
			remain = size
		}
		pkg := CreditPackage{
			Name:        p.PackageCode,
			PackageCode: p.PackageCode,
			Remain:      remain,
			Size:        size,
			Cycle:       true,
		}
		// Used 只在**知道面额**时算（审查 A1-28）：size==0（不限量 / 上游没下发面额）
		// 时 `size-remain` 是负数，面板「已用」列出现 -N，与「剩余 N」自相矛盾。
		// 面额未知时留 0（= 不可算），不要拿负数冒充观测值。
		if size > 0 {
			pkg.Used = size - remain
			if pkg.Used < 0 {
				pkg.Used = 0
			}
		}
		if bp, ok := best[p.PackageCode]; ok {
			if bp.d.PackageName != "" {
				pkg.Name = bp.d.PackageName
			}
			pkg.EndTime = bp.end.Format(time.RFC3339)
			pkg.ExpiresAt = bp.end.UnixMilli()
		}
		remainSum += remain
		totalSum += size
		out = append(out, pkg)
	}
	return out, remainSum, totalSum, nil
}

// globalPackageDetails 拉 paid + free 两份明细并合并。
// 两条都失败 → nil（调用方降级为「无到期批次」）；一条失败只损失那一类的到期信息。
//
// **两条端点的请求体不同形**（2026-10-20 双版本真机抓包实测，CN/GL 完全一致）：
//
//	paid-packages: {PageNumber, PageSize, PackageCodes, Status:[0,3], NeedRenewInfo:true}
//	               —— **没有** SlicePeriod 时间切片
//	free-packages: {PageNumber, PageSize, PackageCodes, Status:[0,3],
//	               SlicePeriodStartTime:<当日> 00:00:00, SlicePeriodEndTime:<当日> 23:59:59}
//	               —— 带时间切片，**没有** NeedRenewInfo
//
// `NeedRenewInfo` 是「是否要返回续订信息」的开关，paid 包靠它拿续订窗口。
// PackageCodes 用 summary 已给的码缩小扫描范围（不按时间盲扫，避免漏包）；
// Status [0,3] = 有效/已过期都要（过期包带权威 DeductionEndTime）。
func (c *Client) globalPackageDetails(a *auth.Auth, codes []string) []packageDetail {
	now := time.Now().In(softRateResetLoc)
	paidBody := map[string]any{
		"PageNumber":    1,
		"PageSize":      200,
		"PackageCodes":  codes,
		"Status":        []int{0, 3},
		"NeedRenewInfo": true,
	}
	freeBody := map[string]any{
		"PageNumber":           1,
		"PageSize":             200,
		"PackageCodes":         codes,
		"Status":               []int{0, 3},
		"SlicePeriodStartTime": cst.Day(now) + " 00:00:00",
		"SlicePeriodEndTime":   cst.Day(now) + " 23:59:59",
	}
	var out []packageDetail
	for _, req := range []struct {
		path string
		body map[string]any
	}{
		{billingPaidPath, paidBody},
		{billingFreePath, freeBody},
	} {
		raw, err := c.billingJSON(a, "POST", req.path, req.body)
		if err != nil {
			continue
		}
		var rep packagesReply
		if json.Unmarshal(billingInner(raw, "Accounts"), &rep) != nil {
			continue
		}
		out = append(out, rep.Accounts...)
	}
	return out
}

// roundCapacity 把上游的（可能是字符串小数形态的）容量四舍五入成 int64。
//
// 为什么不能直接 `int64(x)`（审查 A1-07）：summary 里 CycleRemainCapacity /
// CycleTotalCapacity 是 **"335.47000026" 这样的字符串小数**，直接截断会把 0.9 分
// 余额记成 0 —— 面板显示「余额 0」、该批到期信息整条被丢弃（`r <= 0 → continue`），
// 池子可能把仍有余额的号判成耗尽。口径与 CN 侧 packageRemainUsed 的
// 「四舍五入到最近整数」一致，两条链路不再各说各话。
//
// 越界保护：flexFloat 来自上游文本，可能是 1e300 这种脏值；math.Round 后直接转
// int64 在 Go 里是**未定义行为**（实现相关，常见结果是 -9223372036854775808）。
// 超出 int64 范围时钳到边界，宁可记成「很大」也不要变成负数。
func roundCapacity(v float64) int64 {
	if math.IsNaN(v) {
		return 0
	}
	r := math.Round(v)
	if r >= math.MaxInt64 {
		return math.MaxInt64
	}
	if r <= math.MinInt64 {
		return math.MinInt64
	}
	return int64(r)
}

// balanceRemain 明细自报的剩余额度：Cycle 期套餐优先，回退整包容量；Precise 形态优先。
func (d packageDetail) balanceRemain() float64 {
	prefer := func(precise, plain, preciseCycle, plainCycle flexFloat) float64 {
		if preciseCycle > 0 || plainCycle > 0 {
			if preciseCycle > 0 {
				return float64(preciseCycle)
			}
			return float64(plainCycle)
		}
		if precise > 0 {
			return float64(precise)
		}
		return float64(plain)
	}
	return prefer(d.CapacityRemainPrecise, d.CapacityRemain,
		d.CycleCapacityRemainPrecise, d.CycleCapacityRemain)
}

// packageEndAt 取包的「真实到期时刻」，优先 DeductionEndTime（扣费截止，epoch 毫秒）
// ——它是唯一直达扣费语义的字段。套餐型包的 CycleEndTime 只是**月度计量周期**
// （月底清零），扣费截止却在数年后；用 CycleEndTime 会把长期积分误判成「快到期」
// 而优先烧掉（实测 2026-10-03：「CodeBuddy 个人体验版」cycle=2026-10-31 而
// deduction=2034-12-24，余额 500 分）。回退 ExpiredTime（仅在包失效后才回填）
// → CycleEndTime。都缺 → 无到期（保守：不参与「先烧快到期的」排序）。
// packageEndAt 这一批的到期时刻。第三个返回值表示**是按计量周期推算**的（缺真实扣费截止
// 时的回退）——面板据此标注，别把推算值当权威日期。
func packageEndAt(deductionMS int64, expired, cycleEnd string) (time.Time, bool, bool) {
	if deductionMS > 0 {
		return time.UnixMilli(deductionMS).In(softRateResetLoc), true, false
	}
	if t, ok := parsePackageEndTime(expired); ok {
		return t, true, false
	}
	t, ok := parsePackageEndTime(cycleEnd)
	return t, ok, ok
}
