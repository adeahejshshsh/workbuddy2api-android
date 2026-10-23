package scheduler

// 补跑标记：把「今天已经有哪几类任务补跑过」落成一个小文件，让重启不再重复打上游。
//
// 排程只活在内存里时，进程重启就会被 missedToday 判成「今天的时点全过了、还没跑」，
// 于是每次启动都把签到 / 旅行 / 活跃上报对每个账号各打一遍；一天重开三次就是三倍
// 无意义请求（账号越多、被上游风控盯上的概率越高）。同一思路见 agent2api 的
// task_state.rs：「重启不重置节奏，停机期间只补一次」。
//
// 只记日期不记时刻：精度到「天」就够（missedToday 的判据本来也是按天），文件里
// 最多六行，旧日期写过就丢，不需要清理策略。

import (
	"encoding/json"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
)

// ranToday 报告 kind 今天是否已经补跑过。未配置 RunStampFile 时恒 false（旧行为）。
func (s *Scheduler) ranToday(kind taskKind, now time.Time) bool {
	if s.cfg.RunStampFile == "" {
		return false
	}
	s.stampMu.Lock()
	defer s.stampMu.Unlock()
	return s.loadStamps()[kind.String()] == now.Format("2006-01-02")
}

// markRan 把 kinds 记成「今天已补跑」。落盘失败只记日志：下次重启多补一轮，
// 比让签到状态机因为写盘错误而停摆好。
func (s *Scheduler) markRan(kinds []taskKind, now time.Time) {
	if s.cfg.RunStampFile == "" || len(kinds) == 0 {
		return
	}
	s.stampMu.Lock()
	defer s.stampMu.Unlock()
	day := now.Format("2006-01-02")
	st := s.loadStamps()
	for k, v := range st {
		if v != day {
			delete(st, k) // 只留当天，文件不会越长越大
		}
	}
	for _, k := range kinds {
		st[k.String()] = day
	}
	raw, err := json.Marshal(st)
	if err == nil {
		err = safefile.Write(s.cfg.RunStampFile, raw, 0o600)
	}
	if err != nil {
		log.Printf("scheduler: 补跑标记落盘失败（下次重启会再补一轮）: %v", err)
	}
}

// loadStamps 读标记；文件不在/坏了都当空表（宁可多补一轮，不因此不跑）。
func (s *Scheduler) loadStamps() map[string]string {
	raw, err := safefile.Read(s.cfg.RunStampFile, nil)
	if err != nil {
		return map[string]string{}
	}
	st := map[string]string{}
	if json.Unmarshal(raw, &st) != nil {
		return map[string]string{}
	}
	return st
}
