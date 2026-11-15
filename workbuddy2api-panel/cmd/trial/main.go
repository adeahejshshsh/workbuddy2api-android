// trial 一次性批量领取 global trial 加油包：遍历 auths 下全部账号，
// 仅对 global 账号执行 /billing/ide/trial（CN 无此端点，明确提示不适用）。
//
// 用法：
//
//	# 本地：在项目根目录（需 config.json + auths/ + data/）直接 run
//	go run ./cmd/trial
//
//	# 指定目录 / 关闭 uid 打码
//	go run ./cmd/trial /path/to/auths
//	go run ./cmd/trial -mask=false -dir /path/to/auths
//
//	# 容器内：先 cp 进去再 exec
//	docker cp trial workbuddy2api:/tmp/trial
//	docker exec -w /app workbuddy2api /tmp/trial
//
// 结果逐账号输出到 stdout：
//
//	uid | nick | status | detail
//	----+------+--------+-------
//	... | GLOBAL | OK | trial granted
//	... | GLOBAL | ALREADY | 已领取过（幂等，不算失败）
//	... | CN     | N/A   | not applicable
//
// detail 默认只输出错误类别码（审查 W2-C2-01）：err.Error() 可能带上游响应片段、
// 请求 URL 或文件路径，而 stdout 会进 docker exec 日志/CI 日志/工单附件。
// 排障时用 -debug（或 TRIAL_DEBUG=1）打开原文，原文一律先过脱敏再输出。
package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 错误类别码（审查 W2-C2-01）：报表只输出这些码，原文走 -debug。
const (
	errKindRead    = "E_READ"    // 文件读不出来（权限/IO/半截文件重试后仍失败）
	errKindParse   = "E_PARSE"   // auth 文件解析失败（格式/字段缺失）
	errKindClaim   = "E_CLAIM"   // 上游领取失败（HTTP/业务码）
	errKindUnknown = "E_UNKNOWN" // 未分类
)

// debugDetail 是否输出错误原文（-debug / TRIAL_DEBUG=1）。
var debugDetail bool

// trialError 带类别码的错误（报表只印 kind，原文留在 Unwrap 里给 -debug 用）。
type trialError struct {
	kind string
	err  error
}

func (e *trialError) Error() string { return e.kind }
func (e *trialError) Unwrap() error { return e.err }

// errKindOf 取错误类别码（未包装的一律 E_UNKNOWN）。
func errKindOf(err error) string {
	var te *trialError
	if errors.As(err, &te) && te.kind != "" {
		return te.kind
	}
	return errKindUnknown
}

// failureDetail 生成报表里的 detail 列（审查 W2-C2-01）。
//
// 默认只回类别码；-debug 时追加**脱敏后**的原文（Bearer/access_token/secret
// 之类一律替换成 ***，并截断到 120 字节），保证即便开了调试也不会把凭据写进日志。
func failureDetail(err error) string {
	kind := errKindOf(err)
	if !debugDetail || err == nil {
		return kind
	}
	var te *trialError
	if errors.As(err, &te) && te.err != nil {
		return kind + ": " + redactDetail(te.err.Error())
	}
	return kind + ": " + redactDetail(err.Error())
}

var (
	// reBearer 脱敏 Bearer 令牌（上游错误文案里偶尔会回显请求头）。
	reBearer = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{6,}`)
	// reSecretKV 脱敏 key=value / "key":"value" 形态的凭据字段。
	reSecretKV = regexp.MustCompile(`(?i)"?(access_token|refresh_token|device_token|api_key|secret|password|token)"?\s*[:=]\s*"?[A-Za-z0-9._\-]{6,}"?`)
)

// redactDetail 对错误原文做脱敏 + 截断（审查 W2-C2-01）。
func redactDetail(s string) string {
	s = reBearer.ReplaceAllString(s, "Bearer ***")
	s = reSecretKV.ReplaceAllString(s, "$1=***")
	return logfmt.Truncate(s, 120)
}

// classifyTrial 归一化 ClaimTrial 结果（纯函数，供 main 循环与测试直接断言）：
// err → FAIL；claimed → OK；否则（幂等码已领）→ ALREADY。
func classifyTrial(claimed bool, err error) (trialStatus, string) {
	switch {
	case err != nil:
		// 审查 W2-C2-01：不再把 err.Error() 直接拼进报表（会随 stdout 留存）。
		return trialFailed, failureDetail(&trialError{kind: errKindClaim, err: err})
	case claimed:
		return trialOK, "trial granted"
	default:
		return trialAlready, "already claimed (idempotent)"
	}
}

// trialStatus 单账号 trial 领取结果状态。
type trialStatus string

const (
	trialOK      trialStatus = "OK"
	trialAlready trialStatus = "ALREADY"
	trialNotApp  trialStatus = "N/A"     // CN 账号不适用
	trialUnknown trialStatus = "UNKNOWN" // realm 判不出来（审查 W2-C2-06）：跳过并告警
	trialFailed  trialStatus = "FAIL"
)

type trialRow struct {
	uid    string // 解析成功后的账号 uid；解析失败为空
	file   string // 来源文件名（解析失败时是唯一的定位线索）
	nick   string
	status trialStatus
	detail string
}

// listAuthFiles 返回 auths 目录下待处理的账号文件。
//
// 扫描口径与网关 auth.LoadDirReport 完全一致（审查 W2-C2-03）：os.ReadDir +
// filepath.Match("workbuddy*.json")。不用 filepath.Glob —— 它会把「目录不存在/
// 权限不足」吞成「0 个文件」，脚本看到的是「没有账号」而不是「读不到目录」，
// 而 LoadDirReport 早就因为同一个坑从 Glob 改成了 ReadDir。
func listAuthFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ok, _ := filepath.Match("workbuddy*.json", e.Name()); ok {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// loadAuthFile 读并解析单个 auth 文件，解析失败重读一次（审查 W2-C2-02）。
//
// 为什么重试：网关侧 SaveAtomic 正常路径是 tmp+rename 原子替换，但 bind-mount
// 兜底路径是 O_TRUNC 原地覆写 —— 与本工具并发时可能读到半截 JSON。重读一次
// 即可跨过这个毫秒级窗口，避免把「正在轮换的账号」误判成「账号坏了」。
func loadAuthFile(path string) (*auth.Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &trialError{kind: errKindRead, err: err}
	}
	a, perr := auth.Parse(raw)
	if perr == nil {
		return a, nil
	}
	raw2, err2 := os.ReadFile(path)
	if err2 != nil {
		return nil, &trialError{kind: errKindRead, err: err2}
	}
	a2, perr2 := auth.Parse(raw2)
	if perr2 != nil {
		return nil, &trialError{kind: errKindParse, err: perr}
	}
	return a2, nil
}

// realmOf 账号域三态（审查 W2-C2-06）：显式 realm > domain 推断 > ""（unknown）。
//
// 为什么不能只用 IsGlobal()：up.GlobalEnabled 被本工具强制打开（trial 是 global
// 专属端点），而 Realm() 在「realm 字段缺失 + domain 也缺失」时会回落成 cn ——
// 于是「判不出来」被当成「CN 账号，不适用」静默跳过，或者反过来给 CN 号打
// global 端点（上游 4xx/风控计数）。这里把「判不出来」显式暴露出来。
//
// 推断口径与 auth.BackfillRealm 一致（isGlobalDomain 由 ResolveRealm 复用），
// 不自己另写一套后缀匹配。
func realmOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	if r := strings.TrimSpace(a.RealmStored()); r == "cn" || r == "global" {
		return r
	}
	if strings.TrimSpace(a.Domain) != "" {
		return auth.ResolveRealm("", a.Domain)
	}
	return ""
}

func main() {
	dir := flag.String("dir", "auths", "auths 目录（也可用第一个位置参数）")
	mask := flag.Bool("mask", true, "报表里对 uid 打码（-mask=false 显示完整 uid）")
	debug := flag.Bool("debug", false, "错误详情输出原文（默认只输出类别码）")
	flag.Parse()
	if flag.NArg() > 0 {
		*dir = flag.Arg(0) // 兼容既有用法：trial [authsDir]
	}
	debugDetail = *debug || os.Getenv("TRIAL_DEBUG") == "1"

	files, err := listAuthFiles(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list auth files in %s: %v\n", *dir, err)
		os.Exit(1)
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", *dir)
		os.Exit(1)
	}

	up := upstream.New()
	// trial 是 global 专属端点：必须开启 global realm 路由，否则 upstream.New() 的
	// GlobalEnabled 零值 false 会把请求路由到 CN base（codebuddy.cn）而必然失败。
	up.GlobalEnabled = true
	var rows []trialRow
	for _, f := range files {
		r := trialRow{file: filepath.Base(f)}
		a, err := loadAuthFile(f)
		if err != nil {
			r.status, r.detail = trialFailed, failureDetail(err)
			rows = append(rows, r)
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// 仅 global 账号适用：CN 明确提示不适用，不发任何请求；realm 判不出来
		// 的一律跳过并标 UNKNOWN（审查 W2-C2-06）。
		switch realmOf(a) {
		case "global":
			r.status, r.detail = classifyTrial(up.ClaimTrial(a))
		case "cn":
			r.status, r.detail = trialNotApp, "CN account not applicable"
		default:
			r.status = trialUnknown
			r.detail = "realm unknown (no realm/domain), skipped"
		}
		rows = append(rows, r)
	}

	var okN, alreadyN, notAppN, unknownN, failN int
	fmt.Printf("uid                                  | nick        | status  | detail\n")
	fmt.Printf("-------------------------------------+-------------+---------+------------------------------\n")
	for _, r := range rows {
		fmt.Printf("%-36s | %-11s | %-7s | %s\n",
			trunc(rowIdent(r, *mask), 36), trunc(r.nick, 11), r.status, r.detail)
		switch r.status {
		case trialOK:
			okN++
		case trialAlready:
			alreadyN++
		case trialNotApp:
			notAppN++
		case trialUnknown:
			unknownN++
		default:
			failN++
		}
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d na=%d unknown=%d fail=%d\n",
		len(rows), okN, alreadyN, notAppN, unknownN, failN)
	if *mask {
		fmt.Fprintln(os.Stderr, "提示: uid 已打码（前 8 位）；需要完整 uid 请加 -mask=false")
	}
	if !debugDetail && failN > 0 {
		fmt.Fprintln(os.Stderr, "提示: 失败详情只显示类别码；需要原文请加 -debug（原文会先脱敏）")
	}
}

// rowIdent 报表第一列：解析成功的行用 uid，失败的行用来源文件名
// （解析失败时没有 uid，文件名是唯一能定位到文件的线索）。
func rowIdent(r trialRow, mask bool) string {
	if r.uid != "" {
		return maskUID(r.uid, mask)
	}
	if mask {
		return maskAuthFileName(r.file)
	}
	return r.file
}

// maskUID 报表里对 uid 打码（审查 W2-C2-04）：只留前 8 位，与网关日志的
// logfmt.UID8 同口径（全量 uid 可从 auths/ 文件名或 state.json 反查）。
func maskUID(uid string, mask bool) string {
	if !mask {
		return uid
	}
	return logfmt.UID8(uid)
}

// maskAuthFileName 对解析失败行的文件名打码：保留 workbuddy- 前缀与 .json 后缀，
// 中间只留前 6 位。
//
// 为什么不像 uid 那样只留 8 位：文件名是 `workbuddy-<uid>.json`，直接套 UID8 会
// 把每一行都变成 "workbuddy"，多个失败行在报表里长得一模一样，反而没法排障。
func maskAuthFileName(base string) string {
	if base == "" {
		return "-"
	}
	name := strings.TrimSuffix(base, ".json")
	if !strings.HasPrefix(name, "workbuddy-") {
		// 文件名里不含 uid（如手工维护的 workbuddy.json）：没有可打码的部分。
		return base
	}
	name = name[len("workbuddy-"):]
	if len(name) > 6 {
		name = name[:6] + "…"
	}
	return "workbuddy-" + name + ".json"
}

func trunc(s string, n int) string {
	// 按 rune 边界截断（bug #14）：按字节切会把中文昵称切成半个字符，
	// 终端/日志里出现非法 UTF-8。仓库里早有 logfmt.Truncate 专门做这件事。
	return logfmt.Truncate(s, n)
}
