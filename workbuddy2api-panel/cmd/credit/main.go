// credit.go — WorkBuddy 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//
// 输出结构:
//
//	{"service":"workbuddy","ts":N,
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"uid","nickname","remain","used","size","packages","ok","error?"}]}
//
// 接口与聚合逻辑：POST codebuddy.cn/v2/billing/meter/get-user-resource，聚合所有 package 的
// Cycle* 字段，TotalDosage 作 size 下限。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const billingBaseCN = "https://www.codebuddy.cn"

// billingBaseGlobal 国际版计费域。global 账号打 CN 域会得到 401：
// www.codebuddy.cn 不认 workbuddy.ai 的 token（实测 401，workbuddy.ai 同 token 为 code=0）。
const billingBaseGlobal = "https://www.workbuddy.ai"

// billingBaseFor 按账号 realm 选择计费域。
//
// 判定口径与 internal/auth.Realm() 一致：显式 realm=global 或 domain 落在
// workbuddy.ai 家族，都按国际版处理。cmd/credit 早先对所有账号硬编码 CN 域，
// 导致 global 账号余额查询恒返回 401（面板显示的是池内缓存值，不是实时的）。
func billingBaseFor(af *authFile) string {
	if strings.EqualFold(strings.TrimSpace(af.Auth.Realm), "global") {
		return billingBaseGlobal
	}
	d := strings.ToLower(strings.TrimSpace(af.Auth.Domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return billingBaseGlobal
	}
	return billingBaseCN
}

// UnmarshalJSON 同时吃两种落盘形态（review #17）：嵌套形
// {"auth":{"accessToken":…},"account":{…}} 与扁平形 {"accessToken":…,"uid":…}。
// auth.Parse 已明确支持扁平形，CLI 只认嵌套会让这些账号静默消失。
func (af *authFile) UnmarshalJSON(b []byte) error {
	type nested struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
			Domain      string `json:"domain"`
			Realm       string `json:"realm"`
		} `json:"auth"`
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
		AccessToken  string `json:"accessToken"`
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	}
	var n nested
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	af.Auth.AccessToken, af.Auth.Domain, af.Auth.Realm = n.Auth.AccessToken, n.Auth.Domain, n.Auth.Realm
	af.Account.UID, af.Account.EnterpriseID, af.Account.Nickname = n.Account.UID, n.Account.EnterpriseID, n.Account.Nickname
	// 扁平形：顶层字段补位（嵌套形时这些字段为空，不会覆盖）
	if af.Auth.AccessToken == "" {
		af.Auth.AccessToken = n.AccessToken
	}
	if af.Auth.Domain == "" {
		af.Auth.Domain = n.Domain
	}
	if af.Auth.Realm == "" {
		af.Auth.Realm = n.Realm
	}
	if af.Account.UID == "" {
		af.Account.UID = n.UID
	}
	if af.Account.EnterpriseID == "" {
		af.Account.EnterpriseID = n.EnterpriseID
	}
	if af.Account.Nickname == "" {
		af.Account.Nickname = n.Nickname
	}
	return nil
}

type authFile struct {
	Auth struct {
		AccessToken string `json:"accessToken"`
		Domain      string `json:"domain"`
		Realm       string `json:"realm"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
}

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

type resourcePackage struct {
	CapacityRemain      int64 `json:"CapacityRemain"`
	CapacityUsed        int64 `json:"CapacityUsed"`
	CapacitySize        int64 `json:"CapacitySize"`
	CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64 `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64 `json:"CycleCapacitySize"`
}

// packageRemainUsed 与 billing.go:203-258 一致
func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
		}
		// 与 internal/upstream 的权威口径对齐（bug-3）：先把 used 钳到 size、
		// 再由 used 反算 remain，保证 remain+used == size。缺这两条时上游脏数据
		// （CycleCapacityUsed > Size）会输出「剩余 50 / 已用 150 / 面额 100」。
		if used > size {
			used = size
		}
		remain = size - used
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	// 非周期包同样要钳（bug 3）：上游对账延迟时 CapacityRemain 可能 > Size 或为负，
	// 不钳会让 `credit -pretty` 打印「剩余 150 / 面额 100」，汇总与依赖这份 JSON 的
	// 脚本一起错。口径与 internal/upstream 的同名逻辑一致。
	if remain < 0 {
		remain = 0
	}
	if size > 0 && remain > size {
		remain = size
	}
	if used == 0 && size > remain {
		used = size - remain
	}
	// used 也要钳（BUG-10）：上游 CapacityUsed > Size 时会出现「剩余 50 / 已用 150 / 面额 100」，
	// 与 internal/upstream 的同名逻辑对齐。
	if size > 0 && used > size {
		used = size
	}
	if size > 0 && remain+used > size {
		remain = size - used
	}
	return remain, used, size
}

// fetchEnterpriseResource 企业版 / 静态 API Key 的额度（review #17）：
// 个人 get-user-resource 对二者恒返回空 Accounts，网关侧走
// get-enterprise-user-usage，CLI 此前没有对应分支 → 一律显示 0。
func fetchEnterpriseResource(af *authFile) (remain, used, size int64, packs int, err error) {
	base := billingBaseFor(af)
	client := &http.Client{Timeout: 20 * time.Second}
	req, rerr := http.NewRequest(http.MethodPost, base+"/v2/billing/meter/get-enterprise-user-usage", bytes.NewReader([]byte("{}")))
	if rerr != nil {
		return 0, 0, 0, 0, rerr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+af.Auth.AccessToken)
	resp, rerr := client.Do(req)
	if rerr != nil {
		return 0, 0, 0, 0, rerr
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return 0, 0, 0, 0, fmt.Errorf("http %d: %s", resp.StatusCode, clipStr(string(raw), 120))
	}
	var r struct {
		Credit        float64 `json:"credit"`
		LimitNum      int64   `json:"limitNum"`
		LimitNumSnake int64   `json:"limit_num"`
		UsedNum       float64 `json:"used_num"`
	}
	if uerr := json.Unmarshal(raw, &r); uerr != nil {
		return 0, 0, 0, 0, fmt.Errorf("enterprise parse: %w", uerr)
	}
	limit := r.LimitNum
	if limit == 0 && r.LimitNumSnake != 0 {
		limit = r.LimitNumSnake
	}
	return int64(r.Credit), int64(r.UsedNum), limit, 1, nil
}

// isEnterpriseOrAPIKey 判该账号是否走企业口径：有 enterpriseId，或 token 是
// ck_/pt_ 前缀的静态 API Key（与 auth.IsAPIKey 同判据）。
func isEnterpriseOrAPIKey(af *authFile) bool {
	if strings.TrimSpace(af.Account.EnterpriseID) != "" {
		return true
	}
	t := af.Auth.AccessToken
	return strings.HasPrefix(t, "ck_") || strings.HasPrefix(t, "pt_")
}

func fetchUserResource(af *authFile) (remain, used, size int64, packs int, err error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	base := billingBaseFor(af)
	// 与 internal/upstream 的 billingMeterPaths 同口径（bug 14）：global 域是
	// 「先无 /v2，404 再换带 /v2」的双路径候选；cn 域固定带 /v2。
	// 此前 CLI 只试第一条，上游若只认 /v2，全部 global 号都会报 http 404。
	paths := []string{"/v2/billing/meter/get-user-resource"}
	if base == billingBaseGlobal {
		paths = []string{"/billing/meter/get-user-resource", "/v2/billing/meter/get-user-resource"}
	}
	client := &http.Client{Timeout: 20 * time.Second}
	var resp *http.Response
	for i, path := range paths {
		req, rerr := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
		if rerr != nil {
			return 0, 0, 0, 0, rerr
		}
		req.Header.Set("Authorization", "Bearer "+af.Auth.AccessToken)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		if af.Account.UID != "" {
			req.Header.Set("X-User-Id", af.Account.UID)
		}
		if af.Account.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", af.Account.EnterpriseID)
			req.Header.Set("X-Tenant-Id", af.Account.EnterpriseID)
		}
		if af.Auth.Domain != "" {
			req.Header.Set("X-Domain", af.Auth.Domain)
		}
		resp, rerr = client.Do(req)
		if rerr != nil {
			return 0, 0, 0, 0, rerr
		}
		if resp.StatusCode == http.StatusNotFound && i < len(paths)-1 {
			resp.Body.Close()
			continue // 换下一个候选路径
		}
		break
	}
	if resp == nil {
		return 0, 0, 0, 0, fmt.Errorf("no billing path tried")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					TotalDosage int64             `json:"TotalDosage"`
					Accounts    []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, 0, 0, err
	}
	if env.Code != 0 {
		return 0, 0, 0, 0, fmt.Errorf("code=%d %s", env.Code, env.Msg)
	}
	for _, a := range env.Data.Response.Data.Accounts {
		r, u, s := packageRemainUsed(a)
		remain += r
		used += u
		size += s
	}
	packs = len(env.Data.Response.Data.Accounts)
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := env.Data.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, nil
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	// 扫描口径与网关 auth.LoadDir 对齐（bug 4）：网关用 workbuddy*.json，
	// 只认 workbuddy-*.json 会漏掉手工维护的 workbuddy.json 等，静默少账号。
	files, err := filepath.Glob(filepath.Join(authDir, "workbuddy*.json"))
	// 空目录必须报错退出（bug 12）：此前 err 被 `_` 丢弃且不校验空数组，
	// 空 auths/ 会输出 {"accounts":[]} 并 exit 0 —— 脚本据此分不清
	// 「没登账号」还是「查完了没数据」，与 signin/trial 的行为也不一致。
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", authDir)
		os.Exit(1)
	}
	sort.Strings(files)

	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		var af authFile
		raw, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(raw, &af) != nil {
			continue
		}
		res := accountResult{UID: af.Account.UID, Nickname: af.Account.Nickname}
		if af.Auth.AccessToken == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)
			continue
		}
		var remain, used, size int64
		var packs int
		if isEnterpriseOrAPIKey(&af) {
			remain, used, size, packs, err = fetchEnterpriseResource(&af)
		} else {
			remain, used, size, packs, err = fetchUserResource(&af)
		}
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain = &remain
			res.Used = &used
			res.Size = &size
			res.Packages = packs
			res.OK = true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}

	var totalRemain, totalUsed, totalSize int64
	okCount := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Used != nil {
				totalUsed += *a.Used
			}
			if a.Size != nil {
				totalSize += *a.Size
			}
		}
	}
	out := map[string]any{
		"service": "workbuddy",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"used":     totalUsed,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize, okCount)
		return
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty 人类可读日报：四行汇总，无账号明细。
func printPretty(accounts []accountResult, totalRemain, totalUsed, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("📊 WorkBuddy 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}

// clipStr 截断到 n 字节（CLI 错误文案用；超长上游原文不必整段打出来）。
func clipStr(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
