// Package cst 固定北京时区（UTC+8），作为「日界 / 钟点」的唯一定义。
//
// 为什么必须固定而不是用 time.Local（misakano7545 `025cc107`，借鉴
// aimod-cc/agent2api core::beijing 的复盘）：
//
// 上游的「今天」是**北京时间的自然日**，而网关可能跑在 Docker / NAS / 海外 VPS 上。
// 按宿主时区算，UTC 主机上会静默漂 8 小时：
//   - 排程钟点：配 activity_hours=[10] 实际在 CST 18:00 跑；
//   - 硬冷却「次日 04:00」落在 CST 12:00（签到早结束，白等）；
//   - 夜猫子 23:00–08:00 窗口错位、签到日键跨日、积分包到期批次比较错日。
//
// 用 FixedZone 而不是 time.LoadLocation("Asia/Shanghai")：不依赖宿主 tzdata
// （Android / scratch 镜像里常常没有），且 CST 无夏令时，固定 +8 与 IANA 等价。
//
// 边界：**面向操作者的统计**（用量分桶、请求日志切天）仍用宿主本地时区，
// 那是「用户看到的时间」，与上游语义无关。
package cst

import "time"

// Loc 北京时区（UTC+8，无夏令时）。
var Loc = time.FixedZone("CST", 8*3600)

// Now 当前北京时间。
func Now() time.Time { return time.Now().In(Loc) }

// Today 北京时间的当天 00:00。
func Today() time.Time {
	n := Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, Loc)
}

// DayKey 北京时间的日期键（"2006-01-02"），用于「今天是否已跑过」这类去重。
func DayKey(t time.Time) string { return t.In(Loc).Format("2006-01-02") }

// Day 与 DayKey 同义（上游命名，供 spread 的日确定性哈希使用）。
func Day(t time.Time) string { return DayKey(t) }
