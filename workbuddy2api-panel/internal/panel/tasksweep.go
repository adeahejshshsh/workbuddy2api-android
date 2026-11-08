// tasksweep.go 补领清扫：只 accept 待接受的、只 claim 已达标未领的，不跑任何行为链路。
//
// 动机（上游 issue #174）：达标未领的任务此前没有补领兜底——新号自动跑时它们还没达标
// （awaiting_progress），成长队列一天只跑一次，白名单（autoActions）外的码更是永远
// 进不了队列。本清扫与 autoActions 无关，判据只有 progress，故周期跑即可兜住全部漏领。
//
// 与 taskcenter.go 的队列执行分工：那边跑**行为链路**（真对话 / 真建会话），是重活；
// 这边只做两次列表 + accept + claim，轻到可以每 15 分钟跑一遍。两条路径共用同一个
// per-account 锁（tryLockAccount），不会对同一账号并发动作。
package panel

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// RunTaskSweepNow 一轮清扫（scheduler 周期调用）。返回 (清扫账号数, 摘要)。
//
// 已禁用 / 国际区 / 企业版账号一个上游请求都不发（门控与任务中心扫描一致）；
// 正在跑任务动作的账号本轮让位（与单任务 / 一键完成 / 成长队列共用 per-account 锁）。
func (p *Panel) RunTaskSweepNow() (int, string) {
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return 0, "账号池未接线"
	}
	var swept, busy, accepted, claimed, failed int
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled || st.Realm == "global" {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || !a.SupportsPersonalGrowth() {
			continue // a==nil 无凭证；企业版 / 静态 API Key 无个人成长体系
		}
		if !p.tryLockAccount(st.UID) {
			busy++
			continue
		}
		acc, cl, err := p.sweepAccount(a)
		p.unlockAccount(st.UID)
		swept++
		accepted += acc
		claimed += cl
		if err != nil {
			failed++
			log.Printf("WARN: 补领清扫 %s: %v", logfmt.Label(a.UID, a.NicknameValue()), err)
			continue
		}
		if cl > 0 {
			// 领到奖励就刷积分：与手动补领同一口径（报告 #12），否则面板余额会滞后一轮。
			p.refreshCreditsAsync(a)
		}
	}
	return swept, fmt.Sprintf("清扫 %d 个账号（跳过 %d）· 接受 %d · 领奖 %d · 出错 %d",
		swept, busy, accepted, claimed, failed)
}

// sweepAccount 单账号清扫，返回 (接受数, 领奖数, 列表失败错误)。
//
// 领奖判据只用 progress（Task.Claimable）而不是 accept_status：后者会在回读时短暂转
// completed 随后被整体回滚（见 autotask.go），据此领奖会误领。领奖本身幂等（重复领
// 上游返回 already_claimed → 0 分 0 能），所以逐条失败只记 WARN 不中断，宁可多试一轮。
func (p *Panel) sweepAccount(a *auth.Auth) (accepted, claimed int, err error) {
	label := logfmt.Label(a.UID, a.NicknameValue())
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0, 0, err
	}
	mp, mpErr := p.cfg.Upstream.ListTasksMP(a)
	if mpErr != nil {
		log.Printf("WARN: 补领清扫 %s mp 列表: %v", label, mpErr)
	}

	// 端标记不同，两种口径分开提交（缺 mp 头 accept 返回 task not found）。
	if codes := acceptPendingCodes(tasks); len(codes) > 0 {
		if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
			log.Printf("WARN: 补领清扫 %s 接受: %v", label, err)
		} else {
			accepted += len(codes)
		}
	}
	if codes := acceptPendingCodes(mp); len(codes) > 0 {
		if err := p.cfg.Upstream.AcceptTasksMP(a, codes); err != nil {
			log.Printf("WARN: 补领清扫 %s 接受(mp): %v", label, err)
		} else {
			accepted += len(codes)
		}
	}

	// mp 列表是默认口径的超集，按 task_code 去重（否则同一任务领两次）。
	// 去重用**小写**键：写入与查询必须同口径（报告 #11）——同一任务码的两种大小写
	// 各自通过会让同一奖励被重复发起领奖。
	seen := make(map[string]bool, len(tasks)+len(mp))
	onlyMP := make(map[string]bool, len(mp))
	for _, t := range mp {
		onlyMP[strings.ToLower(t.TaskCode)] = true
	}
	for _, t := range tasks {
		onlyMP[strings.ToLower(t.TaskCode)] = false
	}
	for _, t := range append(append([]upstream.Task{}, tasks...), mp...) {
		key := strings.ToLower(t.TaskCode)
		if t.TaskCode == "" || seen[key] {
			continue
		}
		seen[key] = true
		if !t.Claimable || t.Claimed || t.Locked {
			continue // 锁定 = 上游未解锁（Sequential 族每日解锁一环），领也领不到
		}
		// mp 专属码走 mp 领奖（自带 Web 域 400 降级）；只在 mp 列表出现的码同办，
		// 否则 Web 域对 mp 任务 400 会变成每轮一条 WARN。
		var cerr error
		if isMPTaskCode(t.TaskCode) || onlyMP[key] {
			_, _, _, cerr = p.cfg.Upstream.ClaimRewardMP(a, t.TaskCode)
		} else {
			_, _, _, cerr = p.cfg.Upstream.ClaimReward(a, t.TaskCode)
		}
		if cerr != nil {
			log.Printf("WARN: 补领 %s %s: %v", label, t.TaskCode, cerr)
			continue
		}
		claimed++
	}
	return accepted, claimed, nil
}

// acceptPendingCodes 待接受的任务码：未领、未达标、未锁定、状态还不是 accepted/completed。
func acceptPendingCodes(tasks []upstream.Task) []string {
	var codes []string
	for i := range tasks {
		t := &tasks[i]
		if !t.Claimed && !t.Locked && !taskProgressReached(t) &&
			t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	return codes
}

// taskProgressReached 服务端确认进度达标（与 upstream 同判据）：
// 进度满、或 accept_status=completed、或 status=complete。
//
// 不复用 Task.Claimable：那一位含「未领取」，而 accept 阶段恰恰要判断
// 「已达标未领取」（该 accept 但不该 claim 的形态不该被 accept 跳过）。
func taskProgressReached(t *upstream.Task) bool {
	return t != nil && (t.Target > 0 && t.Current >= t.Target ||
		t.AcceptStatus == "completed" || strings.EqualFold(t.Status, "complete"))
}

// ClaimSweepOnce 扫一遍全池，把「已达标未领」的成长任务奖励补领掉（面板立即按钮）。
//
// 与 RunTaskSweepNow 同一执行体（sweepAccount）：此前这里是**第二份实现**（只 claim
// 不 accept、无 per-account 锁、无 mp 超集去重），两条路径的口径会各自漂移。
// 并发 3 是手动入口的既有语义（人要等结果），周期任务那边是串行 + 让位。
func (p *Panel) ClaimSweepOnce(ctx context.Context) (accounts, claimed, failed int) {
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return 0, 0, 0
	}
	sem := make(chan struct{}, 3)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled || st.Realm == "global" {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || !a.SupportsPersonalGrowth() {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("ERR: [panel] claim sweep panic uid=%s: %v", uid, r)
				}
			}()
			if !p.tryLockAccount(uid) {
				return // 该号正在跑任务动作：本轮让位，等下一轮
			}
			_, cl, err := p.sweepAccount(a)
			p.unlockAccount(uid)
			mu.Lock()
			accounts++
			claimed += cl
			if err != nil {
				failed++
			}
			mu.Unlock()
			if cl > 0 {
				p.refreshCreditsAsync(a)
			}
		}(a, st.UID)
	}
	wg.Wait()
	return
}
