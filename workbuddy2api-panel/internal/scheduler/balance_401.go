package scheduler

// 余额探测 401 的「登录态求证」（借鉴 fork Albert-PZY 9f264ca + c6a3900 / issue #215）：
// 余额刷新是池内唯一的分钟级健康观测量，但拿到错误此前只写一行日志、不碰池状态——
// 上游批量注销会话后，号在 billing/chat 两侧都已 401，面板仍显示「可用」，要等每日
// 22:00 的保活连败满阈（≈3 天）才转 disabled，运维期间看不到任何信号。
//
// 不能把 401 直接当 session dead 计罚：401 有两种成因，
//  1. 会话被上游注销（refresh 回 12153）→ 需重登，应计罚；
//  2. 降级 token（issue #215 评论实测：billing 恒 401、chat 仍可用）→ 号还能干活，计罚会误杀。
// 用 refresh 做一次权威求证即可分开。非 401 不参与：5xx/限流/传输层各有既有惩罚路径。

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func (s *Scheduler) noteBalanceAuthFailure(a *auth.Auth, uid string, probeErr error) {
	var ue *upstream.Error
	if !errors.As(probeErr, &ue) || ue.Status != http.StatusUnauthorized {
		return
	}
	// 没有 RT 无从求证（静态 key / 企业号等形态）：保持「只记日志」的旧语义。
	if a.RefreshTokenValue() == "" {
		return
	}
	label := logfmt.Label(uid, a.Nickname)
	// 退避只记「refresh 成功」：真死号每轮 refresh 都回 12153 且不轮换 RT，必须每轮都
	// 试，连续计数才能按预期在阈值内判死；把失败也记进退避会让死号永远停在 1/阈。
	if !s.allowAuthHeal(uid) {
		log.Printf("balance %s: 401（%s 内已求证过登录态，本轮跳过 refresh）", label, authHealBackoff)
		return
	}
	// 账号级族锁（审查 E4）：保活族也会对这个号 refresh。两条路径并发轮换同一个 RT 时，
	// 后到的那个拿着已作废的 RT 去换 → 上游回 12153 → 一个健康号被判成 session dead。
	if !s.TryBeginAccountTask(uid, TaskKeepalive) {
		log.Printf("balance %s: 该号正在保活刷新，本轮跳过", label)
		return
	}
	defer s.EndAccountTask(uid, TaskKeepalive)
	rerr := s.cfg.Upstream.RefreshToken(a)
	if rerr == nil {
		// 落盘用带重试的版本（审查 C7-080）：RT 已在服务端轮换，磁盘上还是作废的
		// 旧 RT —— 一次 SaveAtomic 失败就意味着重启后这个号要重新登录。保活路径
		// 早就是 SaveAtomicRetry，这里此前是单次写入，两处口径不一。
		if serr := a.SaveAtomicRetry(); serr != nil {
			log.Printf("balance %s: 401 后刷新成功但落盘失败: %v", label, serr)
		}
		s.noteAuthHeal(uid)
		s.cfg.Pool.ClearSessionDead(uid)
		log.Printf("balance %s: 401 经 refresh 求证为降级 token（会话仍活），已换新 token 并清 session dead 计数", label)
		return
	}
	var re *upstream.Error
	// Kind 之外再看一次 code=12153：doJSON 只用 env.Msg 做分类，而上游的 12153 文案
	// 不总是英文原文（本地化后不含 marker 就会落成 ErrClient，死号便永远不计罚）。
	if errors.As(rerr, &re) && (re.Kind == upstream.ErrSessionDead || strings.Contains(re.Msg, "12153")) {
		// refresh token 失效 ≠ 账号不可用：AT 未过期时 chat 仍正常（issue #83，
		// 与 keepalive 同一判据）。计死会在连续 3 轮后禁用掉一个健康账号。
		if upstream.SessionDeadButUsable(a) {
			// 也写退避（审查 C7-081）：这类号 AT 还能用，但 RT 已经死了，refresh
			// 注定失败。不写退避 = 每 30 分钟一轮的余额刷新都会再打一次 refresh，
			// 一天 ~48 次注定失败的上游请求（还是轮换 RT 的写接口）。
			// 死号不受影响：真死号走不到这个分支（SessionDeadButUsable 为 false），
			// 仍然每轮都试，连续计数才能在阈值内判死。
			s.noteAuthHeal(uid)
			log.Printf("balance %s: refresh token 已失效但 access token 仍有效，不计 session dead（%s 内不再求证）", label, authHealBackoff)
			return
		}
		if s.cfg.Pool.NoteSessionDead(uid) {
			log.Printf("balance %s: 连续 %d 次 401 且 refresh 判死 — 禁用（需重新登录）", label, pool.SessionDeadThreshold())
			return
		}
		if st, ok := s.cfg.Pool.Status(uid); ok {
			log.Printf("balance %s: 401 且 refresh 判死（session dead 计数 %d/%d）", label, st.SessionDeadFails, pool.SessionDeadThreshold())
		}
		return
	}
	log.Printf("balance %s: 401 但 refresh 失败（非 session dead，不计罚）: %v", label, rerr)
}

// authHealBackoff 同一账号 401→refresh 求证的最小间隔。降级 token 每轮探测都会 401，
// 若每轮都刷新，一天会轮换 288 次 RT。
const authHealBackoff = 30 * time.Minute

// allowAuthHeal 退避窗口判定。
func (s *Scheduler) allowAuthHeal(uid string) bool {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if t, ok := s.authHealAt[uid]; ok && time.Since(t) < authHealBackoff {
		return false
	}
	return true
}

// noteAuthHeal 记录一次「refresh 求证」的时刻（成功，或判定为「RT 死但 AT 可用」）。
func (s *Scheduler) noteAuthHeal(uid string) {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if s.authHealAt == nil {
		s.authHealAt = make(map[string]time.Time)
	}
	now := time.Now()
	// 写入时清掉已过退避窗口的条目（审查 C7-082）：这张表只用来判「这个号最近是否
	// 求证过」，过期条目没有任何信息量，而它此前只写不删 —— 长期运行 + 账号轮换
	// 会让 map 随历史 uid 数单调增长。
	for k, t := range s.authHealAt {
		if now.Sub(t) >= authHealBackoff {
			delete(s.authHealAt, k)
		}
	}
	s.authHealAt[uid] = now
}
