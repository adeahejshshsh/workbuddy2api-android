package panel

// 导出凭据的往返契约：导出的文件必须能原样导回去（换包名迁移就靠这一条）。
//
// 只断言「响应里有 uid」是不够的：refresh_token / device_token / realm 任何一个在
// 导出或解析里丢了，导入后账号能显示但第一次刷新就掉线，用户要到第二天才发现。

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// exportFixture 两个 CN 号（一个企业号、一个被禁用）+ 一个没有 refresh token 的占位号。
func exportFixture(t *testing.T) (*Panel, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	pl := pool.New("")

	a1 := &auth.Auth{UID: "u-1", AccessToken: "AT-1", RefreshToken: "RT-1",
		ExpiresAt: 1893456000, Domain: "www.codebuddy.cn", Nickname: "老王",
		EnterpriseID: "ent-9", DeviceToken: "dt-1"}
	a2 := &auth.Auth{UID: "u-2", AccessToken: "AT-2", RefreshToken: "RT-2",
		ExpiresAt: 1893456000, Domain: "workbuddy.ai", Nickname: "intl"}
	if _, err := auth.BackfillRealmFor(a2, "global"); err != nil {
		t.Fatalf("realm: %v", err)
	}
	// 没有 refresh token 的号：导出必须跳过（导出去也只是一份 401 文件）
	a3 := &auth.Auth{UID: "u-3", AccessToken: "AT-3", Domain: "www.codebuddy.cn"}

	for _, a := range []*auth.Auth{a1, a2, a3} {
		a.FilePath = dir + "/workbuddy-" + a.UID + ".json"
		pl.Add(a)
	}
	pl.Disable("u-1", "11140 request illegal")

	return New(Config{Version: "test", Pool: pl, AuthDir: dir}), pl
}

// TestExportAccountsRoundTrip 导出 → 解析 → 导入 → 回读，凭据一个字段都不能少。
func TestExportAccountsRoundTrip(t *testing.T) {
	p, _ := exportFixture(t)

	rec := httptest.NewRecorder()
	p.exportAccounts(rec, httptest.NewRequest("GET", "/panel/api/export/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 凭据文件绝不能被缓存
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control 应含 no-store，实际 %q", got)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "workbuddy-accounts-") {
		t.Fatalf("Content-Disposition 文件名不对: %q", cd)
	}

	var exported []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &exported); err != nil {
		t.Fatalf("导出不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	if len(exported) != 2 {
		t.Fatalf("应导出 2 个有 refresh token 的号（占位号跳过），实际 %d: %s", len(exported), rec.Body.String())
	}

	// 导出体直接喂给导入解析器：这就是「复制 → 粘贴 → 导入」的实际链路
	accs, err := parseImportPayload(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("导出的文件导不回去: %v", err)
	}
	if len(accs) != 2 {
		t.Fatalf("解析出 %d 条", len(accs))
	}
	byUID := map[string]cockpitAccount{}
	for _, a := range accs {
		byUID[a.UID] = a
	}
	if a := byUID["u-1"]; a.RefreshToken != "RT-1" || a.DeviceToken != "dt-1" ||
		a.EnterpriseID != "ent-9" || a.Nickname != "老王" {
		t.Fatalf("u-1 字段丢失: %+v", a)
	}
	if a := byUID["u-2"]; a.Realm != "global" || a.Domain != "workbuddy.ai" {
		t.Fatalf("u-2 realm/domain 丢失（国际版号会被判回 cn）: %+v", a)
	}

	// 再走一遍真正的导入落盘 + 回读，确认磁盘上的凭据也完整
	dir2 := t.TempDir()
	// 落盘这一段复用解析器的结果构造 Auth：把导出的数组塞进导入 handler 需要的
	// multipart 太绕，而 import 的核心就是「解析 → SaveAtomic → 池里可用」。
	for _, a := range accs {
		au := &auth.Auth{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken,
			ExpiresAt: a.ExpiresAt, Domain: a.Domain, UID: a.UID, Nickname: a.Nickname,
			DeviceToken: a.DeviceToken, EnterpriseID: a.EnterpriseID}
		au.FilePath = dir2 + "/workbuddy-" + a.UID + ".json"
		if err := au.SaveAtomic(); err != nil {
			t.Fatalf("save %s: %v", a.UID, err)
		}
	}
	loaded, err := auth.LoadDir(dir2)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("回读 %d 个账号", len(loaded))
	}
	for _, a := range loaded {
		switch a.UID {
		case "u-1":
			if a.RefreshToken != "RT-1" || a.DeviceToken != "dt-1" {
				t.Fatalf("u-1 落盘后凭据不全: %+v", a)
			}
		case "u-2":
			if a.Realm() != "global" {
				t.Fatalf("u-2 realm 落盘后变成 %q", a.Realm())
			}
		}
	}
}

// TestExportAccountsOmitsPlaceholders 没有 refresh token 的占位号不导出。
func TestExportAccountsOmitsPlaceholders(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "only-uid"})
	p := New(Config{Version: "test", Pool: pl, AuthDir: t.TempDir()})

	rec := httptest.NewRecorder()
	p.exportAccounts(rec, httptest.NewRequest("GET", "/panel/api/export/accounts", nil))
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("占位号不该被导出，实际: %s", rec.Body.String())
	}
}

// TestExportThenImportEndToEnd 端到端：面板导出的文件，原样上传给导入接口能不能吃下。
//
// 这是「导出的文件能直接被本应用导入吗」的验收判据 —— 之前的往返用例只验证了
// parseImportPayload（解析器），没有经过真正的 handler（multipart、字段校验、
// 落盘路径、入池）。这两层之间最容易出的岔子是「解析器认、handler 不认」。
func TestExportThenImportEndToEnd(t *testing.T) {
	src, _ := exportFixture(t)

	// ① 导出
	rec := httptest.NewRecorder()
	src.exportAccounts(rec, httptest.NewRequest("GET", "/panel/api/export/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("导出失败 code=%d", rec.Code)
	}
	exported := rec.Body.Bytes()

	// ② 原样当成文件上传给导入接口（用户实际就是这么做的：从「下载」里选那个 json）
	dir2 := t.TempDir()
	dst := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir2})

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "workbuddy-accounts-20261107.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(exported); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec2 := httptest.NewRecorder()
	dst.importCockpit(rec2, req)

	if rec2.Code != http.StatusOK {
		t.Fatalf("导入被拒 code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var res struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &res); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if res.Imported != 2 || res.Skipped != 0 {
		t.Fatalf("导入结果 imported=%d skipped=%d errors=%v", res.Imported, res.Skipped, res.Errors)
	}

	// ②b 再导一次（用户手滑重复导入）：同名覆盖，不能变成 4 个号
	body.Reset()
	mw = multipart.NewWriter(&body)
	if fw, err = mw.CreateFormFile("file", "workbuddy-accounts-20261107.json"); err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(exported)
	mw.Close()
	req2 := httptest.NewRequest("POST", "/panel/api/import/cockpit", &body)
	req2.Header.Set("Content-Type", mw.FormDataContentType())
	dst.importCockpit(httptest.NewRecorder(), req2)
	if n := len(dst.cfg.Pool.List()); n != 2 {
		t.Fatalf("重复导入后池里应有 2 个号（同名覆盖），实际 %d", n)
	}

	// ③ 新池子里的号必须带着完整凭据（能直接干活，不用重新登录）
	loaded, err := auth.LoadDir(dir2)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("落盘 %d 个账号", len(loaded))
	}
	for _, a := range loaded {
		if a.UID == "u-1" {
			if a.RefreshToken != "RT-1" || a.DeviceToken != "dt-1" || a.EnterpriseID != "ent-9" {
				t.Fatalf("u-1 导入后凭据不全: %+v", a)
			}
		}
		if a.UID == "u-2" && a.Realm() != "global" {
			t.Fatalf("u-2 realm 丢了: %q", a.Realm())
		}
	}
}

// TestExportImportKeepsAccountType 导出/导入必须带上 account.type，且导入后判定不变。
//
// 现场：国服个人号常带一个 enterpriseId（个人空间/默认租户）。只要 type 在导出→导入
// 这一圈里丢了，新机器上就会按「enterpriseId 非空」把它判成企业版 —— 面板显示企业版，
// 签到与成长任务被门控掉。所以 type 必须跟着走，并且判定以它为准。
func TestExportImportKeepsAccountType(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New("")
	a := &auth.Auth{UID: "p1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 1893456000, Domain: "www.codebuddy.cn",
		AccountType: "personal", EnterpriseID: "ent-cn-default", Nickname: "国服个人号"}
	a.FilePath = filepath.Join(dir, "workbuddy-p1.json")
	pl.Add(a)

	src := New(Config{Version: "test", Pool: pl, AuthDir: dir})
	rec := httptest.NewRecorder()
	src.exportAccounts(rec, httptest.NewRequest("GET", "/panel/api/export/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("导出失败: %d", rec.Code)
	}

	accs, err := parseImportPayload(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("导出体解析失败: %v", err)
	}
	if len(accs) != 1 || accs[0].Type != "personal" {
		t.Fatalf("type 没随导出走: %+v", accs)
	}

	// 走一遍真实导入：落盘后必须仍是个人号
	dir2 := t.TempDir()
	dst := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir2})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "export.json")
	_, _ = fw.Write(rec.Body.Bytes())
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	dst.importCockpit(httptest.NewRecorder(), req)

	loaded, err := auth.LoadDir(dir2)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("回读失败: %v n=%d", err, len(loaded))
	}
	if loaded[0].AccountType != "personal" {
		t.Fatalf("导入后 type 丢失: %q", loaded[0].AccountType)
	}
	if loaded[0].IsEnterprise() {
		t.Fatal("personal 账号导入后被判成企业版（本次要修的 bug）")
	}
}
