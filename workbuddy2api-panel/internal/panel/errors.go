// errors.go 错误观测：把日志环形缓冲里的 WARN/ERR 行按「账号 × 错误类型」聚合。
//
// 为什么不是再开一个日志流：原始日志里几十行同质 WARN，人眼抓不到真正重要的差异
// （某个号报错 44 次、另一个只 1 次）。按 (账号, 类型) 聚合成十几行才读得动。
//
// 数据源：面板日志环（Ring）—— 不新增存储，日志本来就在内存里（Ring 的 Text 已剥
// 行首时间戳，故时刻取 LogEntry.TS，不从文本里解析）。
package panel

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// ErrStat 单条「账号 × 错误类型」聚合行。
type ErrStat struct {
	Account string `json:"account"`          // 打印用的账号标签（logfmt.Label 的「昵称(uid8)」）；空 = 非账号级
	Kind    string `json:"kind"`             // 人话类型
	Code    string `json:"code,omitempty"`   // 上游业务码（6004/11102/12153…）
	Model   string `json:"model,omitempty"`  // 相关模型
	Count   int    `json:"count"`            // 累计次数
	Last    string `json:"last,omitempty"`   // 最近一次时刻 HH:MM:SS
	Sev     string `json:"sev"`              // bad | warn | info
	Until   string `json:"until,omitempty"`  // 模型级限流的解除时刻（上游文案里的重置时间）
	Sample  string `json:"sample,omitempty"` // 表格正文样本（截断，供一眼认出类别）
	// SampleFull 与 Sample 同一行原文，只放宽到 sampleFullBytes 供 title 悬停看全文。
	// 分开两个字段：正文要短才不撑爆表格，而 160B 常把上游 body 的 msg 切掉一半
	// （issue #61：悬停也看不到剩下的，因为 title 原先复用了已截断的 Sample）。
	SampleFull string `json:"sample_full,omitempty"`
}

const (
	sampleBytes     = 160  // 表格正文上限
	sampleFullBytes = 1024 // title 全文上限：够装上游 body JSON，又不让 5s 轮询载荷失控
)

const (
	sevBad  = "bad"  // 需处理：限流/额度/会话失效/WAF/传输失败
	sevWarn = "warn" // 需留意：内容拦截、解析失败、目录拉取、续写降级
	sevInfo = "info" // 正常但值得知道：轮转退避、保底让位
)

// 从本仓真实日志行里抽字段（样本见 errors_test.go）。
var (
	reUpstream = regexp.MustCompile(`upstream (\d{3}) ([a-z_]+)`) // WARN: [upstream] chat_stream acct=X: upstream 429 soft_rate body=…
	reAcct     = regexp.MustCompile(`acct=([^\s:]+)`)             // acct=昵称(a1b2c3d4) / acct=u1
	reModel    = regexp.MustCompile(`model=([^\s:,]+)`)           // model=glm-5.2（尾随 :/, 是句读，不是模型名）
	reCode     = regexp.MustCompile(`"code"\s*:\s*"?(\d+)"?`)     // body 里的业务码
	reReset    = regexp.MustCompile(`将在\s*(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})|will reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
	// 调度族失败行（scheduler）：`WARN: keepalive 昵称(uid8): <err>` —— 账号标签紧跟任务名，
	// 没有 acct= 前缀，账号要从这里取。
	reTask = regexp.MustCompile(`^(?:WARN: |ERR: )?(checkin|keepalive|balance|travel|activity|user-resource|streak-bonus|lottery) ([^:]+):`)
)

// taskKinds 调度任务名 → 错误类型（人话）。
var taskKinds = map[string]string{
	"checkin":       "签到失败",
	"keepalive":     "保活失败",
	"balance":       "余额查询失败",
	"travel":        "猫猫旅行失败",
	"activity":      "活跃上报失败",
	"user-resource": "账号资源查询失败",
	"streak-bonus":  "连登兑换/抽奖失败",
	"lottery":       "抽奖失败",
}

// kindOf 把一行归类为「人话类型 + 上游码 + 严重度」。空类型 = 不参与聚合。
//
// 为什么不直接用上游原文当类型：原文带 requestId / 时间戳，同一类错误会被拆成 N 条，
// 聚合就失效了。
func kindOf(line string) (kind, code string, sev string) {
	// 客户端掐断（Stop / 掉线）不是网关也不是上游故障，但同一原因散在多种文案里
	// （transport error / upstream parse failed / 续写失败 / rotate backoff aborted）。
	// 先归到 info，否则这些噪声会把真故障挤出汇总首屏（线上实测 54 条聚合里 37 条是它）。
	if strings.Contains(line, "context canceled") || strings.Contains(line, "ctx cancelled") ||
		strings.Contains(line, "context cancelled") {
		return "客户端中断", "", sevInfo
	}
	if m := reUpstream.FindStringSubmatch(line); m != nil {
		code = m[1]
		switch m[2] {
		case "soft_rate", "hard_credit":
			kind, sev = "限流/额度", sevBad
		case "session_dead":
			kind, sev = "会话失效", sevBad
		case "waf_block":
			kind, sev = "WAF 拦截", sevBad
		case "server":
			kind, sev = "上游 5xx", sevBad
		case "account_fault":
			kind, sev = "账号授权/配额故障", sevBad
		case "model_blocked":
			kind, sev = "模型不可用", sevBad
		case "content_blocked":
			kind, sev = "内容拦截", sevWarn
		case "not_found":
			kind, sev = "上游 404", sevWarn
		case "bad_params", "client", "model_param_invalid", "prompt_too_long", "image_invalid":
			kind, sev = "请求参数错误", sevInfo // 请求本身的问题：不罚账号、不影响他人
		default:
			kind, sev = "上游 "+m[2], sevWarn
		}
	} else {
		switch {
		case strings.Contains(line, "transport error"):
			kind, sev = "传输失败", sevBad
		case strings.Contains(line, "read body"):
			kind, sev = "响应体读取失败", sevBad
		case strings.Contains(line, "upstream timeout"):
			kind, sev = "上游超时（未罚号）", sevBad
		case strings.Contains(line, "waf ip-level block"):
			kind, sev = "WAF IP 级拦截", sevBad
		case strings.Contains(line, "save auth failed"):
			kind, sev = "凭证落盘失败", sevWarn
		case strings.Contains(line, "empty upstream stream"):
			kind, sev = "上游空流", sevWarn
		case strings.Contains(line, "upstream error frame before first byte"):
			kind, sev = "首帧否决（换号）", sevWarn
		case strings.Contains(line, "upstream parse failed"):
			kind, sev = "上游流解析失败", sevWarn
		case strings.Contains(line, "[continue]"):
			kind, sev = "续写降级", sevWarn
		case strings.Contains(line, "[narration]"):
			// 预告句救援：模型停在「……：」上被续写。与 toolonly 分开计数，运维才能
			// 判断是哪条规则在动（issue #209 的建议 3）。
			kind, sev = "续写救援（预告句）", sevInfo
		case strings.Contains(line, "[toolonly]"):
			// 「说一句就停」的救援：命中说明客户端刚才差点看到中途停下，救回来是好事，
			// 但要能算出救了多少、救回的是工具调用还是只有正文——按 info 记，进「救援」一栏。
			kind, sev = "续写救援（说一句就停）", sevInfo
		case strings.Contains(line, "[starved]"):
			kind, sev = "续写救援（思考吃光预算）", sevInfo
		case strings.Contains(line, "fetch models"), strings.Contains(line, "global models"),
			strings.Contains(line, "models.dev fetch"), strings.Contains(line, "model.json"):
			kind, sev = "模型目录拉取异常", sevWarn
		case strings.Contains(line, "credit floor"):
			kind, sev = "积分保底让位", sevInfo
		case strings.Contains(line, "本次大前缀未命中"):
			kind, sev = "缓存重算（费用）", sevWarn
		case strings.Contains(line, "rotate backoff aborted"):
			kind, sev = "轮转退避中止", sevInfo
		default:
			if m := reTask.FindStringSubmatch(line); m != nil {
				kind, sev = taskKinds[m[1]], sevWarn
				// 连续 12153 禁用是账号级终态，比"某次保活失败"严重。
				if strings.Contains(line, "session dead") {
					sev = sevBad
				}
			} else {
				return "", "", ""
			}
		}
	}
	// 模型级限流单列：6004 与账号级 429 的处置完全不同（换模型 vs 等冷却）。
	if strings.Contains(line, "6004") {
		kind = "模型限流(6004)"
	}
	if strings.Contains(line, "11102") {
		kind, code = "模型不存在(11102)", "11102"
	}
	if m := reCode.FindStringSubmatch(line); m != nil && code == "" {
		code = m[1]
	}
	return kind, code, sev
}

// LogLine 聚合输入（Text + 该行时刻）。
type LogLine struct {
	Text string
	TS   time.Time
}

// AggregateErrors 把日志行聚合成「账号 × (类型, 模型)」统计，按 严重度 → 次数 降序
// （最该看的排最前）。非 WARN/ERR 行不参与；WARN/ERR 但没匹配到规则的行归入
// 「其他（未归类）」，绝不静默丢弃。
func AggregateErrors(lines []LogLine) []ErrStat {
	type key struct{ acct, kind, model string }
	agg := map[key]*ErrStat{}
	var order []key

	for _, ln := range lines {
		txt := strings.TrimSpace(ln.Text)
		// WARN/ERR 之外，只额外放行「续写救援」这三族（它们是 info 级的好消息，
		// 但必须能数出来，且按入口分开）：其余 INFO 噪音不进汇总。
		rescue := strings.Contains(txt, "[toolonly]") || strings.Contains(txt, "[narration]") ||
			strings.Contains(txt, "[starved]")
		if !rescue && !strings.HasPrefix(txt, "WARN") && !strings.HasPrefix(txt, "ERR") {
			continue
		}
		kind, code, sev := kindOf(txt)
		if kind == "" {
			// 未归类 ≠ 不重要：新增日志文案时要当场看见它，而不是从汇总里静默消失
			// （这类漏报最难发现——汇总看着正常，错的只是它少了一行）。
			kind, sev = "其他（未归类）", sevWarn
		}
		acct := ""
		if m := reAcct.FindStringSubmatch(txt); m != nil {
			acct = m[1]
		} else if m := reTask.FindStringSubmatch(txt); m != nil {
			acct = strings.TrimSpace(m[2]) // 调度族：账号标签紧跟任务名
		}
		model := ""
		if m := reModel.FindStringSubmatch(txt); m != nil {
			model = m[1]
		}
		until := ""
		if m := reReset.FindStringSubmatch(txt); m != nil {
			for _, g := range m[1:] {
				if g != "" {
					until = g
				}
			}
		}
		k := key{acct, kind, model}
		st, ok := agg[k]
		if !ok {
			st = &ErrStat{Account: acct, Kind: kind, Code: code, Model: model,
				Sev: sev, Sample: logfmt.Truncate(txt, sampleBytes),
				SampleFull: logfmt.Truncate(txt, sampleFullBytes)}
			agg[k] = st
			order = append(order, k)
		}
		st.Count++
		if until != "" {
			st.Until = until // 后写的覆盖：更接近当前状态
		}
		if !ln.TS.IsZero() {
			st.Last = ln.TS.Format("15:04:05")
		}
	}

	out := make([]ErrStat, 0, len(agg))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	rank := map[string]int{sevBad: 0, sevWarn: 1, sevInfo: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Sev] != rank[out[j].Sev] {
			return rank[out[i].Sev] < rank[out[j].Sev]
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Account < out[j].Account
	})
	return out
}
