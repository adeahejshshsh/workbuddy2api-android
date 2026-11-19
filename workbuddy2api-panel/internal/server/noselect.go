package server

// 选号失败的可观测性。
//
// 2026-11-13 的教训：池子里一个号都挑不出来时，网关只回客户端一个 503，服务端日志里
// **什么都没有**——那天 11:00~23:30 有 4,036 条这样的 503（try=0、acct=-、连上游都没打），
// 运维侧直到朋友反馈「用着用着就断」才可能察觉。原因不是账号状态坏了，而是连坐暂停
// （restrict.go）把整个域停了，可「选号返回 nil」的其它成因（禁用/冷却/在途满/积分保底/
// 域过滤）同样无声。
//
// 这里补一条按域限流的 WARN，带分项计数：既能事后回答「那天是谁挡的」，也能在面板日志里
// 直接看出池子是不是被某一类状态掏空。限流是必须的——一个卡住的池子每秒能产生几十条。

import (
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// noSelectLogInterval 同一域两条「选不到号」日志的最小间隔。
const noSelectLogInterval = 60 * time.Second

var (
	noSelectMu   sync.Mutex
	noSelectLast = map[string]time.Time{}
)

// logNoSelectable 池子里挑不出可派发的账号时记一条（按域限流，每分钟最多一条）。
// 拿不到池子（未配置）时静默返回，不影响请求路径。
//
// why 由调用方按 pick 的判据顺序算好（pool.NoPickForRealm）：这条日志的价值全在
// 「是哪一类把号挡完的」—— 只报 healthy>0 仍然得靠人去猜（issue #276）。
func (h *Handler) logNoSelectable(realm, model string, modelBlocked bool, why pool.NoPick) {
	if h.cfg.Pool == nil {
		return
	}
	key := realm
	now := time.Now()
	noSelectMu.Lock()
	if last, ok := noSelectLast[key]; ok && now.Sub(last) < noSelectLogInterval {
		noSelectMu.Unlock()
		return
	}
	noSelectLast[key] = now
	noSelectMu.Unlock()

	log.Printf("WARN: [pool] no selectable account realm=%q model=%q no_pick{%s} model_blocked=%v —— 该域本请求快速失败（503）；healthy>0 说明是全被在途/积分保底/保留积分/冷却挡住",
		realm, model, why.String(), modelBlocked)
}
