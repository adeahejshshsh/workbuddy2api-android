// familylock.go 账号级 × 任务族 的互斥（审查 E4）。
//
// 现有的 `taskLocks` 是**族级**（整个 checkin 族一把锁），它只能挡「两轮全池排程同时跑」，
// 挡不住「手动按号入口 vs 排程」这类同账号同族的并发 —— 两条路径会同时对这个号发起同一批
// 上游写请求。实测到的后果：
//
//   - 手动旅行与排程旅行并发 → 同一天重复兑换；
//   - 补签循环与 21:00 轮 TOCTOU → 同一个号被签两次；
//   - 余额 401 的 refresh 与保活族的 refresh 并发轮换同一个 RT → 一个健康号被判 session dead；
//   - 后台窗口探针与面板探针同时打同一个额度窗口 → 探针请求翻倍（每次都是真实计费）。
//
// 键是 uid + 族：**不同账号之间不互斥**（按号端点存在的意义就是让外部调度器做到每号独立
// 随机时点，叠加全局锁等于把指纹加回去）。
package scheduler

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// TaskWindowProbe 额度窗口探针族（后台循环与面板手动触发共用一把账号锁）。
const TaskWindowProbe = "windowprobe"

// maxFamilyLocks 账号级锁表的软上限。条目数 = 账号数 × 族数（几十 × 8），正常远达不到；
// 超了才做一次清理，避免「账号反复增删」把它撑成泄漏。
const maxFamilyLocks = 4096

// familyLockIdle 空闲多久的锁条目可以被清理。
const familyLockIdle = 10 * time.Minute

// familyLockRefDeleted refs 的「已作废」哨兵（W4-04）。prune 用 CAS(0 → 该值)
// 抢占删除权：只有抢到的一方才能把条目从表里摘掉，抢不到说明有人正持有/正要持有。
const familyLockRefDeleted = -1

type familyLock struct {
	mu   sync.Mutex
	used atomic.Int64 // 最近一次使用时刻（unix nano）
	// refs 引用计数（W4-04）：>0 = 有调用方正拿着指针（持锁中、等锁中、或只读探测中）。
	//
	// 为什么需要它：旧的 pruneLocked 用 `TryLock → Unlock → delete` 判「没人持有」，
	// Unlock 与 delete 之间是可抢占窗口 —— 第三人进来看到条目已被删，新建一把空锁
	// 并**成功持有**，于是同号同族真的双跑了（两路并发 RefreshToken 同一 RT →
	// 12153 → 健康号被计死禁用，正是本文件要防的事故）。
	// refs 必须在「拿到指针」时就 +1，而不是拿到锁之后才 +1：等锁的那一方虽然还没
	// 持有 mu，但已经认定了这把条目，删掉它同样会让后来者拿到另一把锁。
	refs atomic.Int64
	// held 该条目的 mutex 当前是否由 TryBeginAccountTask 成功持有（W4-04）。
	//
	// EndAccountTask 必须先 CAS(true→false) 再 Unlock：对零值/已释放的 mutex
	// 无条件 Unlock 会 panic（sync: unlock of unlocked mutex），而它几乎总是通过
	// defer 调用，panic 发生在收尾阶段，goroutine 里没有 recover 点。
	held atomic.Bool
}

// acquire 尝试占一个引用（refs+1）。返回 false = 条目已被 prune 作废，调用方
// 必须重新从表里 Load（绝不能继续用旧指针）。
func (l *familyLock) acquire() bool {
	for {
		r := l.refs.Load()
		if r < 0 {
			return false
		}
		if l.refs.CompareAndSwap(r, r+1) {
			return true
		}
	}
}

func (l *familyLock) release() { l.refs.Add(-1) }

// familyLocks 账号级族锁表。
//
// 用 sync.Map 而不是 map+Mutex（W4-04）：删除必须走 CompareAndDelete —— 只有
// 「表里这个键仍然指向这个指针」时才删，杜绝「判定与删除之间被换掉」的形态；
// 同时读路径（每次任务开始）不再抢全局锁。
type familyLocks struct {
	// m key → *familyLock
	m sync.Map
	// n 条目数（sync.Map 没有 len）。只用于 maxFamilyLocks 的软上限判定。
	n atomic.Int64
}

// key 账号级族键。用 \x00 分隔，uid 里不会出现该字节。
func familyKey(uid, family string) string { return uid + "\x00" + family }

// count 当前条目数（测试与上限判定用）。
func (f *familyLocks) count() int { return int(f.n.Load()) }

// each 遍历全部条目。sync.Map.Range 的键是 any，这里收敛成 string 便于测试与排障。
func (f *familyLocks) each(fn func(k string, l *familyLock)) {
	f.m.Range(func(k, v any) bool {
		fn(k.(string), v.(*familyLock))
		return true
	})
}

// entry 取（必要时新建）某 (uid, family) 的锁条目，**返回时已持有 1 个引用**。
// 调用方必须在用完后 release()：TryBeginAccountTask 成功时由 EndAccountTask 释放，
// 失败时立即释放。
func (f *familyLocks) entry(uid, family string) *familyLock {
	k := familyKey(uid, family)
	for {
		if v, ok := f.m.Load(k); ok {
			l := v.(*familyLock)
			if !l.acquire() {
				// 条目已被 prune 作废：回到循环顶重新 Load（此刻表里要么没有，
				// 要么是别人新建的另一把锁）。
				continue
			}
			// 回读校验（W4-04）：acquire 与 Load 之间条目可能刚好被 CompareAndDelete
			// 摘掉（prune 看到的是旧的 refs==0）。表里已经不是同一个指针就必须重来，
			// 否则会拿着一把「表里不存在」的锁，而另一个人拿到新表项 —— 又是双跑。
			if cur, ok := f.m.Load(k); ok && cur == v {
				l.used.Store(time.Now().UnixNano())
				return l
			}
			l.release()
			continue
		}
		l := &familyLock{}
		l.refs.Store(1)
		if _, loaded := f.m.LoadOrStore(k, l); loaded {
			// 并发新建：对方赢了，丢掉自己这个空壳重试（refs 归零即可，没人看得见它）。
			l.refs.Store(0)
			continue
		}
		if f.n.Add(1) > maxFamilyLocks {
			f.pruneLocked()
		}
		l.used.Store(time.Now().UnixNano())
		return l
	}
}

// pruneLocked 清掉空闲且当前没人持有的条目。
//
// 名字保留历史（原实现要求调用方持 f.mu）；现在表是 sync.Map，自身并发安全，
// 这里的「Locked」只剩「允许在持锁上下文里调用」的意思。
// W4-04：判据从 `TryLock→Unlock→delete` 换成 `refs CAS(0→作废) + CompareAndDelete`。
func (f *familyLocks) pruneLocked() {
	cutoff := time.Now().Add(-familyLockIdle).UnixNano()
	f.each(func(k string, l *familyLock) {
		if l.used.Load() > cutoff {
			return
		}
		// 先抢占删除权：refs 必须恰好是 0（没人持有、也没人正等着拿）。
		if !l.refs.CompareAndSwap(0, familyLockRefDeleted) {
			return
		}
		if !f.m.CompareAndDelete(k, l) {
			// 表项已被替换（理论上只有本函数会删，属于防御分支）：撤销作废标记，
			// 免得把一个仍在使用的新条目永久标死。
			l.refs.Store(0)
			return
		}
		f.n.Add(-1)
	})
}

// TryBeginAccountTask 尝试占用「某账号的某任务族」。false = 该号该族正在跑。
//
// 调用方必须在成功后 defer EndAccountTask（成对使用；未配 uid/family 时恒 true）。
func (s *Scheduler) TryBeginAccountTask(uid, family string) bool {
	if s == nil || uid == "" || family == "" {
		return true
	}
	l := s.family.entry(uid, family)
	if !l.mu.TryLock() {
		// 没抢到：立刻把引用还回去，否则这个条目永远 prune 不掉（refs 泄漏）。
		l.release()
		return false
	}
	l.held.Store(true)
	l.used.Store(time.Now().UnixNano())
	return true
}

// EndAccountTask 释放 TryBeginAccountTask 占用的锁。
//
// W4-04：先校验「确实是持有者」再 Unlock —— 未 Begin 就 End（或重复 End）时
// 对零值 mutex 的 Unlock 会 panic，且这条路径基本都在 defer 里，没人能 recover。
// 校验失败只记一条 WARN 并返回，不影响调用方收尾。
func (s *Scheduler) EndAccountTask(uid, family string) {
	if s == nil || uid == "" || family == "" {
		return
	}
	k := familyKey(uid, family)
	v, ok := s.family.m.Load(k)
	if !ok {
		log.Printf("WARN: [scheduler] EndAccountTask 找不到族锁条目 uid=%s family=%s（未持有或已被清理），忽略", shortUID(uid), family)
		return
	}
	l := v.(*familyLock)
	if !l.held.CompareAndSwap(true, false) {
		log.Printf("WARN: [scheduler] EndAccountTask 未持有族锁 uid=%s family=%s，忽略（避免 unlock 零值 panic）", shortUID(uid), family)
		return
	}
	l.used.Store(time.Now().UnixNano())
	l.mu.Unlock()
	l.release()
}

// AccountTaskRunning 报告某账号某族是否在跑（只读，不占锁）。
func (s *Scheduler) AccountTaskRunning(uid, family string) bool {
	if s == nil || uid == "" || family == "" {
		return false
	}
	l := s.family.entry(uid, family)
	defer l.release()
	if l.mu.TryLock() {
		l.mu.Unlock()
		return false
	}
	return true
}
