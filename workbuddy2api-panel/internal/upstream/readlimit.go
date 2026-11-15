// readlimit.go 出站响应体的「有界读取 + 截断检测」唯一入口。
//
// **为什么需要这一层**（审查 A1-15 / W4-U2-104）：本包原先有十来处
//
//	raw, err := io.ReadAll(io.LimitReader(resp.Body, N))
//
// 这种写法有两个静默失效面：
//
//  1. `LimitReader` 读到上限就返回 EOF，**调用方无从区分「上游刚好发了 N 字节」
//     与「上游发了 10 倍 N、被我们砍掉了」**。于是截断被当成正常读完继续往下走：
//     JSON 解析报 `unexpected end of JSON input`，排障时指向「上游返回了坏 JSON」，
//     而真因是网关自己砍的（search 的 webfetch 正文最容易撞上，客户端拿到的
//     是半截字符串 —— 不是合法 JSON，也不是可用的 markdown）。
//  2. 截断后仍按完整报文做错误分类：业务码/风控文案常位于报文尾部，被砍掉就
//     `Classify` 判不出 → 罚号/轮转方向跟着错（把 11140 风控当普通 4xx 换号重试）。
//
// 统一口径：**读到上限+1 字节就判定截断**，调用方必须显式决定「报错」还是
// 「降级」，不能再靠「解析失败」间接发现。截断本身不是上游错误，所以不在这里
// 造 *Error —— 由各调用点按自己的语义决定（JSON 接口一律报错：半截 JSON 无论
// 如何都解析不出来，报错只是把归因说清楚）。
package upstream

import (
	"errors"
	"io"
)

// ErrResponseTooLarge 响应体超过该调用点的读取上限（报文被网关截断，不是上游的问题）。
// 调用方可用 errors.Is 判定，避免把「我们砍了」误报成「上游给了坏数据」。
var ErrResponseTooLarge = errors.New("upstream response body exceeds read limit")

// readAllLimited 读至多 max 字节，并回报是否发生截断。
//
// 返回 (data, truncated, err)：
//   - truncated=true 时 data 是**前 max 字节**（仍可给错误分类/日志用，但绝不能当完整报文解析）；
//   - err 非 nil 时 data 无意义（传输层失败）。
//
// 实现上读 max+1 字节：多读的那 1 字节就是「还有后续」的判据。不要图省事只读 max
// ——那正是原写法无法区分「刚好读满」与「被砍」的根因。
func readAllLimited(r io.Reader, max int64) ([]byte, bool, error) {
	if max <= 0 {
		return nil, false, errors.New("readAllLimited: max must be > 0")
	}
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > max {
		return raw[:max], true, nil
	}
	return raw, false, nil
}

// readAllLimitedStrict 同 readAllLimited，但截断直接转成 ErrResponseTooLarge。
// 适合「半截报文必然不可用」的调用点（JSON 接口、需要完整结构的解析）。
func readAllLimitedStrict(r io.Reader, max int64) ([]byte, error) {
	raw, truncated, err := readAllLimited(r, max)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrResponseTooLarge
	}
	return raw, nil
}
