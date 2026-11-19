// rescue_trace.go 救援判据的枚举化诊断（issue #245 的补充诉求）。
//
// 「finish=stop + 声明了工具 + 零工具调用」在日志里一律表现为「没有续写」，
// 但成因差得很远：正文本来就写完了、尾部判定认成写完、历史里没有工具轮、
// 上游没给 usage 所以不敢救……排障时无法区分，复现的用户又不愿交出提示词与正文。
//
// 所以这里只记**枚举码与布尔量**，一个字节的请求内容都不落日志。
// 默认关闭（WB2A_RESCUE_TRACE=1 打开）：判据在热路径上，留一个 if 就够。
//
// 本仓口径：凡是会多打上游或改判据的功能一律默认关。这条只多打一行日志、不改任何判据，
// 但仍然默认关——它唯一的用途是排障时临时打开，常开只是白占日志。
package server

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// rescueTrace 是否记录救援被哪道闸门挡掉。
//
// 环境变量两套名字都认：WB2A_ 是本仓的前缀（见 cmd/server/config.go 那一串 WB2A_*），
// WB2API_RESCUE_TRACE 是上游原提交里的名字——线上按旧名字设的人不至于白设一次。
var rescueTrace = os.Getenv("WB2A_RESCUE_TRACE") != "" || os.Getenv("WB2API_RESCUE_TRACE") != ""

// skip 记下「这条救援被哪道闸门挡掉」后返回 false。
//
// 所有判据出口都走这里，而不是在判据旁边再写一份记录：两份实现会漂移，
// 而这条日志的唯一价值就是「它记的就是当时真正生效的那道闸门」。
// 开关关着时只多一次布尔判断：不拼字符串、不 append，零分配。
func (r *continueReader) skip(rule, code string) bool {
	if rescueTrace {
		r.rescueWhy = append(r.rescueWhy, rule+"="+code)
	}
	return false
}

// logRescueSkip 一轮结束、一条救援都没成时打一行原因码。
//
// prompt 同时给出「有没有 usage」与取值——toolonly 的救援要求 prompt 够长，
// 「上游没给 usage」与「给了但太短」是两种完全不同的结论。
func (r *continueReader) logRescueSkip() {
	if !rescueTrace || len(r.rescueWhy) == 0 {
		return
	}
	prompt := "unknown"
	if p, ok := r.usage["prompt_tokens"].(float64); ok {
		prompt = fmt.Sprintf("%d", int64(p))
	}
	// limit_set / budget_raised 回答「输出预算是不是显式设的」：前者是客户端自己带的额度，
	// 后者是网关把额度抬到了档位下限（生效上限不是客户端写的数）。
	log.Printf("[rescue] acct=%s model=%s finish=%q text=%dB prompt=%s limit_set=%v budget_raised=%v why=%s",
		r.label(), bareModelOf(r.body), r.finish, r.text.Len(),
		prompt, r.limitSet, r.budgetWasRaised(), strings.Join(r.rescueWhy, " "))
}

// 今日各类救援的触发次数（面板概况「今日救援」卡片）。
//
// 为什么需要：每条救援都要多打一次上游、多付一遍整段 prompt（issue #273 实测单条
// 0.2 → 8.58 credits），触发频率此前只在网关日志里能看见，面板上看不出来。计数器让
// 「今天怎么突然贵了」有个一眼能看到的出口。
//
// 进程内计数、跨天归零：它是异常探测器，不是账（重启归零可以接受，真账在用量记录里）。
var rescueToday struct {
	mu  sync.Mutex
	day string
	n   map[string]int
}

func noteRescue(kind string) {
	day := time.Now().Format("2006-01-02")
	rescueToday.mu.Lock()
	defer rescueToday.mu.Unlock()
	if rescueToday.day != day || rescueToday.n == nil {
		rescueToday.day, rescueToday.n = day, map[string]int{}
	}
	rescueToday.n[kind]++
}

// RescueToday 今日各类救援次数快照（narration/toolonly/starved）；跨天自动归零。
func RescueToday() map[string]int {
	day := time.Now().Format("2006-01-02")
	rescueToday.mu.Lock()
	defer rescueToday.mu.Unlock()
	if rescueToday.day != day || rescueToday.n == nil {
		return map[string]int{}
	}
	out := make(map[string]int, len(rescueToday.n))
	for k, v := range rescueToday.n {
		out[k] = v
	}
	return out
}
