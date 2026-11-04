package upstream

// 国际版计费新口径（billing_summary.go）：summary 给总额、paid/free 明细补到期。
//
// 这组用例钉住三件事：① 字符串小数形态能解析（上游 summary 用 "335.47000026"）；
// ② 同一 PackageCode 的多条周期明细只贡献**一次**余额、且只认最早未来到期那条
// （不归并会把同一笔余额重复计入 N 次）；③ 明细全失败时总额仍在（降级为「没有
// 到期批次」），绝不因为明细失败把余额也丢掉。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestFlexFloatParsesStringAndNumber(t *testing.T) {
	var got struct {
		A flexFloat `json:"a"` // 字符串小数
		B flexFloat `json:"b"` // JSON 整数
		C flexFloat `json:"c"` // null
		D flexFloat `json:"d"` // 脏值
	}
	raw := []byte(`{"a":"335.47000026","b":1200,"c":null,"d":"oops"}`)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if float64(got.A) < 335.47 || float64(got.A) > 335.48 {
		t.Fatalf("字符串小数解析错: %v", float64(got.A))
	}
	if float64(got.B) != 1200 {
		t.Fatalf("整数解析错: %v", float64(got.B))
	}
	if got.C != 0 || got.D != 0 {
		t.Fatalf("null/脏值应为 0: %v %v", float64(got.C), float64(got.D))
	}
}

func globalAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
}

// summary 给总额、明细按 PackageCode 对齐补到期；同一码多条周期明细只算一次。
func TestGlobalUserResourceMergesSummaryAndDetails(t *testing.T) {
	soon := 30 * 24 * time.Hour
	future := time.Now().Add(10 * 24 * time.Hour).UnixMilli()  // 10 天后到期（在 soon 内）
	later := time.Now().Add(200 * 24 * time.Hour).UnixMilli()  // 200 天后到期（不在 soon 内）
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Packages":[
				{"PackageCode":"PA","CycleTotalCapacity":"1000","CycleRemainCapacity":"600"},
				{"PackageCode":"PB","CycleTotalCapacity":"500","CycleRemainCapacity":"500"}
			]}}}}`), nil
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-paid-packages"):
			// PA 出现两条（两个计量周期），最早那条 10 天后到期 → 只认它。
			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
				{"PackageCode":"PA","PackageName":"付费包A","DeductionEndTime":`+strconv.FormatInt(future, 10)+`,"CapacityRemain":600},
				{"PackageCode":"PA","PackageName":"付费包A","DeductionEndTime":`+strconv.FormatInt(later, 10)+`,"CapacityRemain":600}
			]}}}}`), nil
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-free-packages"):
			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
				{"PackageCode":"PB","PackageName":"免费包B","DeductionEndTime":`+strconv.FormatInt(later, 10)+`,"CapacityRemain":500}
			]}}}}`), nil
		}
		return nil, errors.New("unexpected path: " + r.URL.Path)
	})
	c.GlobalEnabled = true

	remain, total, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(globalAuth(), soon)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1100 || total != 1500 {
		t.Fatalf("remain/total=%d/%d want 1100/1500", remain, total)
	}
	// 只有 PA（10 天后到期）落在 soon 内；PB 在 200 天后。
	if expiring != 600 {
		t.Fatalf("expiring=%d want 600（只算 soon 内到期的 PA）", expiring)
	}
	if earliestAt.IsZero() || earliestRemaining != 600 {
		t.Fatalf("earliest=%v remaining=%d want PA 那一批", earliestAt, earliestRemaining)
	}
	// 去重：PA 的两条周期明细只算一次（重复计入会变成 1200）。
	if expiring == 1200 {
		t.Fatal("同一 PackageCode 的多条周期明细被重复计入")
	}
}

// 明细两条都失败：总额仍要给（降级为「没有到期批次」）。
func TestGlobalUserResourceSurvivesDetailFailure(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/get-user-resource-summary") {
			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Packages":[
				{"PackageCode":"PA","CycleTotalCapacity":"800","CycleRemainCapacity":"300"}
			]}}}}`), nil
		}
		return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
	})
	c.GlobalEnabled = true

	remain, total, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(globalAuth(), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("明细失败不该让整次查询失败: %v", err)
	}
	if remain != 300 || total != 800 {
		t.Fatalf("remain/total=%d/%d want 300/800", remain, total)
	}
	if expiring != 0 || !earliestAt.IsZero() || earliestRemaining != 0 {
		t.Fatalf("无明细时应无到期批次：expiring=%d earliest=%v remaining=%d", expiring, earliestAt, earliestRemaining)
	}
}

// summary 主口径失败：整次失败（明细单独拿到也没总额可对）。
func TestGlobalUserResourceFailsWhenSummaryFails(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `{"code":500,"msg":"boom"}`), nil
	})
	c.GlobalEnabled = true
	if _, _, _, _, _, err := c.UserResourceDetailedWithExpiry(globalAuth(), 0); err == nil {
		t.Fatal("summary 失败应返回错误")
	}
}

// 逐包构成：remain/size 来自 summary，EndTime 来自明细。
func TestGlobalCreditPackages(t *testing.T) {
	end := time.Now().Add(5 * 24 * time.Hour).UnixMilli()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Packages":[
				{"PackageCode":"PA","CycleTotalCapacity":"1000","CycleRemainCapacity":"400"}
			]}}}}`), nil
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-paid-packages"):
			body, _ := io.ReadAll(r.Body)
			// paid 必须带 NeedRenewInfo、不带时间切片（真机实测的请求体形状）。
			if !bytes.Contains(body, []byte(`"NeedRenewInfo":true`)) {
				return nil, errors.New("paid body 缺 NeedRenewInfo: " + string(body))
			}
			if bytes.Contains(body, []byte("SlicePeriodStartTime")) {
				return nil, errors.New("paid body 不该带时间切片: " + string(body))
			}
			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
				{"PackageCode":"PA","PackageName":"付费包A","DeductionEndTime":`+strconv.FormatInt(end, 10)+`,"CapacityRemain":400}
			]}}}}`), nil
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-free-packages"):
			body, _ := io.ReadAll(r.Body)
			// free 反之：带时间切片、不带 NeedRenewInfo。
			if !bytes.Contains(body, []byte("SlicePeriodStartTime")) {
				return nil, errors.New("free body 缺时间切片: " + string(body))
			}
			if bytes.Contains(body, []byte("NeedRenewInfo")) {
				return nil, errors.New("free body 不该带 NeedRenewInfo: " + string(body))
			}
			return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
		}
		return nil, errors.New("unexpected path: " + r.URL.Path)
	})
	c.GlobalEnabled = true

	pkgs, remain, total, err := c.CreditPackages(globalAuth())
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(pkgs) != 1 || remain != 400 || total != 1000 {
		t.Fatalf("pkgs=%+v remain=%d total=%d", pkgs, remain, total)
	}
	p := pkgs[0]
	if p.Name != "付费包A" || p.PackageCode != "PA" || p.Remain != 400 || p.Size != 1000 || p.Used != 600 {
		t.Fatalf("逐包字段不对：%+v", p)
	}
	if p.ExpiresAt == 0 || p.EndTime == "" {
		t.Fatalf("到期应由明细补上：%+v", p)
	}
}
