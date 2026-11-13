package upstream

// 签到回执（daily-checkin）的分类与解析。
//
// 为什么需要单独一层：签到接口有三种「HTTP 上都算成功」的形态，事实却完全不同——
// 真的签上了（上游发积分）、今天早就签过（上游不发积分）、上游拒签。只把「有没有
// 报错」往上交，面板只能二选一：把幂等拒绝说成「签到成功」，或者把成功说成失败。
// 同类项目（workbuddy2api-hub #875）在生产 1288 条签到记录里翻出 250 条幂等拒绝
// 被报成「签到成功 (+100积分)」，根因就是只看了 code 分支、没看文案。
//
// 本仓库线上实测（2026-11-12 07:08 CST，CN 号 00000002，同一号连打两次）：
//
//	首次  HTTP 200 {"credit":100,"streak_days":44,"is_streak_day":false}
//	重复  HTTP 400 {"code":10001,"msg":"今天已签到，请明天再来","requestId":"…"}
//
// 两个要点：
//  1. 成功回执是**平铺**的（没有 {code,msg,data} 信封），所以必须拿原始 body 解析，
//     只交 env.Data 会得到 null —— 积分数就从这里丢了；
//  2. 失败回执的 HTTP 状态是 400，doJSONRaw 在 >=400 时不解信封，code/msg 只能从
//     原始 body 里读，不能靠 *Error.Msg（那是截断到 200 字的整段原文）。

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// CheckinState 一次签到调用的结果归属。
type CheckinState string

const (
	// CheckinChecked 本次真的签上了，上游已发积分。
	CheckinChecked CheckinState = "checked"
	// CheckinAlready 今天早些时候已签过，本次一个积分都没发。
	CheckinAlready CheckinState = "already"
	// CheckinPending 上游「请求处理中」：没签成，但可以稍后重试（不是终态）。
	CheckinPending CheckinState = "pending"
	// CheckinFailed 上游拒签（业务错误 / 风控 / 参数）。
	CheckinFailed CheckinState = "failed"
)

// checkinFailureMarkers 「这次没签成」的硬词。只有这些词能压过 already：
// 「签到失败，已签到天数不足 7 天」同时含「失败」和「已签到」，语义是失败，
// 不能被 already 抢走（同类项目踩过的坑，判定顺序必须 failed 先于 already）。
var checkinFailureMarkers = []string{"失败", "fail", "not started", "未开启", "不支持"}

// CheckinReceipt 一次签到调用的回执。
type CheckinReceipt struct {
	State CheckinState
	// Credit 本次真实发放的积分。CreditKnown 为假时 Credit 无意义（上游没带回这个字段），
	// 调用方不得把「0」当成「发了 0 分」展示，更不得拿硬编码的 100 顶上。
	Credit      int
	CreditKnown bool
	StreakDays  int
	Code        int
	Msg         string
}

// CheckinReceipt 执行签到并回执分类结果。
// 返回的 error 语义与 DailyCheckin 一致（非 checked/already 都算错），方便老调用方
// 只看 error 就够；需要区分「本次真签到」和「今天早就签过」的调用方读 receipt。
func (c *Client) CheckinReceipt(a *auth.Auth) (CheckinReceipt, error) {
	var rec CheckinReceipt
	err := c.retryBillingTransient(func() error {
		env, raw, e := c.billingMeterRaw(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
		rec = classifyCheckin(env, raw, e)
		return e
	})
	return rec, err
}

// classifyCheckin 把一次签到回执归到四态之一。
//
// 判定顺序（顺序本身是正确性的一部分）：pending → failed → already → checked。
//   - pending 最先：「请求处理中，请勿重复操作」是明确没签成，且不能落盘；
//   - failed 先于 already：见 checkinFailureMarkers 注释；
//   - already 先于 checked：code=0 但文案是「今天已签到」的形态（另一版本上游实测）
//     是幂等拒绝，不是本次签到成功。
func classifyCheckin(env apiEnvelope, raw []byte, err error) CheckinReceipt {
	code, msg := envelopeCodeMsg(env, raw)
	rec := CheckinReceipt{State: CheckinFailed, Code: int(code), Msg: msg}
	if err != nil {
		if rec.Msg == "" {
			rec.Msg = err.Error()
		}
		text := strings.ToLower(rec.Msg)
		switch {
		case containsAnyText(text, billingProcessingMarkers):
			rec.State = CheckinPending
		case containsAnyText(text, checkinFailureMarkers):
			rec.State = CheckinFailed
		case IsAlreadyCheckin(err):
			rec.State = CheckinAlready
		}
		return rec
	}
	if containsAnyText(strings.ToLower(rec.Msg), alreadyCheckinMarkers) {
		rec.State = CheckinAlready
		return rec
	}
	rec.State = CheckinChecked
	parseCheckinCredit(&rec, env.Data, raw)
	return rec
}

// parseCheckinCredit 取本次真实发放的积分。优先 data 信封，其次平铺 body
// （daily-checkin 成功回执是平铺的，见文件头注释）。
func parseCheckinCredit(rec *CheckinReceipt, data, raw []byte) {
	type creditBody struct {
		Credit     *int `json:"credit"`
		StreakDays int  `json:"streak_days"`
	}
	try := func(b []byte) bool {
		if len(b) == 0 {
			return false
		}
		var v creditBody
		if json.Unmarshal(b, &v) != nil {
			return false
		}
		if v.Credit != nil {
			rec.Credit, rec.CreditKnown = *v.Credit, true
		}
		if v.StreakDays > 0 {
			rec.StreakDays = v.StreakDays
		}
		return rec.CreditKnown
	}
	if try(data) {
		return
	}
	try(raw)
}

// envelopeCodeMsg 从信封或原始 body 里取业务 code 与 msg 文案。
// HTTP >= 400 时 doJSONRaw 不解信封（env 为零值），只能从 raw 读；
// 成功时回执大多平铺，env 的 code/msg 可能也是空的。
func envelopeCodeMsg(env apiEnvelope, raw []byte) (flexInt, string) {
	code, msg := env.Code, env.Msg
	if code != 0 && msg != "" {
		return code, msg
	}
	var v struct {
		Code flexInt `json:"code"`
		Msg  string  `json:"msg"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &v) == nil {
		if v.Code != 0 {
			code = v.Code
		}
		if v.Msg != "" {
			msg = v.Msg
		}
	}
	// 兜底形态：{code,msg,message}（部分端点用 message 而非 msg）。
	if msg == "" && len(raw) > 0 {
		var v2 struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &v2) == nil {
			msg = v2.Message
		}
	}
	return code, strings.TrimSpace(msg)
}

// containsAnyText 任一短语命中即真（大小写不敏感）。lowerText 必须已 ToLower。
func containsAnyText(lowerText string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lowerText, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
