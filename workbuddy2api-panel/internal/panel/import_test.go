package panel

// 导入凭据的三种形状（换包名后「旧包的 auths/*.json 搬不过来」的补救路径）。
//
// 判据是**导入后能读回来**：解析器只做形状映射，真正的风险是字段在映射里悄悄丢了
// （device_token / realm / enterpriseId 都是「丢了不报错、事后才发作」的那类），
// 所以端到端用例走一遍 auth.LoadDir 回读比对。

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// nativeAuthJSON 本仓 auths/workbuddy-<uid>.json 的真实形状（嵌套形）。
const nativeAuthJSON = `{
  "auth": {
    "accessToken": "AT-abc",
    "refreshToken": "RT-abc",
    "expiresAt": 1893456000,
    "domain": "www.codebuddy.cn",
    "realm": "cn"
  },
  "account": {
    "uid": "u-1001",
    "enterpriseId": "",
    "nickname": "老王"
  },
  "device_token": "dt-xyz"
}`

// TestImportPayloadNativeNested 单个 auths 文件（嵌套形）：字段一个都不能丢。
func TestImportPayloadNativeNested(t *testing.T) {
	accs, err := parseImportPayload([]byte(nativeAuthJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(accs) != 1 {
		t.Fatalf("应解析出 1 个账号，实际 %d", len(accs))
	}
	a := accs[0]
	if a.UID != "u-1001" || a.AccessToken != "AT-abc" || a.RefreshToken != "RT-abc" {
		t.Fatalf("核心字段错: %+v", a)
	}
	if a.ExpiresAt != 1893456000 || a.Domain != "www.codebuddy.cn" || a.Realm != "cn" {
		t.Fatalf("令牌元数据错: %+v", a)
	}
	if a.Nickname != "老王" {
		t.Fatalf("昵称错: %q", a.Nickname)
	}
	if a.DeviceToken != "dt-xyz" {
		t.Fatalf("device_token 丢了（导入后会被当新设备，指纹漂）: %+v", a)
	}
}

// TestImportPayloadCockpitArray cockpit tools 导出（snake_case 数组）必须继续能用。
func TestImportPayloadCockpitArray(t *testing.T) {
	raw := `[{"id":"1","uid":"u-2002","nickname":"n2","access_token":"AT-2",
	          "refresh_token":"RT-2","expires_at":1893456000,"domain":"workbuddy.ai"}]`
	accs, err := parseImportPayload([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(accs) != 1 || accs[0].UID != "u-2002" || accs[0].AccessToken != "AT-2" {
		t.Fatalf("cockpit 形状被改坏: %+v", accs)
	}
}

// TestImportPayloadNativeFlat 扁平形（手写/旧版）也认。
func TestImportPayloadNativeFlat(t *testing.T) {
	raw := `{"accessToken":"AT-3","refreshToken":"RT-3","expiresAt":1893456000,
	         "domain":"www.codebuddy.cn","uid":"u-3003","nickname":"n3"}`
	accs, err := parseImportPayload([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(accs) != 1 || accs[0].UID != "u-3003" || accs[0].AccessToken != "AT-3" {
		t.Fatalf("扁平形解析错: %+v", accs)
	}
}

// TestImportPayloadMixedArrayAndWrapper 混合数组与 {"accounts":[…]} 包装。
func TestImportPayloadMixedArrayAndWrapper(t *testing.T) {
	mixed := `[` + nativeAuthJSON + `,{"uid":"u-2002","access_token":"AT-2","refresh_token":"RT-2"}]`
	accs, err := parseImportPayload([]byte(mixed))
	if err != nil {
		t.Fatalf("混合数组解析失败: %v", err)
	}
	if len(accs) != 2 || accs[0].UID != "u-1001" || accs[1].UID != "u-2002" {
		t.Fatalf("混合数组结果错: %+v", accs)
	}

	wrapped := `{"accounts":[{"uid":"u-2002","access_token":"AT-2","refresh_token":"RT-2"}]}`
	accs, err = parseImportPayload([]byte(wrapped))
	if err != nil {
		t.Fatalf("包装形解析失败: %v", err)
	}
	if len(accs) != 1 || accs[0].UID != "u-2002" {
		t.Fatalf("包装形结果错: %+v", accs)
	}
}

// TestImportPayloadRejectsGarbage 垃圾输入要给明确错误，不能静默导入 0 个。
func TestImportPayloadRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", "<xml/>", `"just a string"`} {
		if _, err := parseImportPayload([]byte(raw)); err == nil {
			t.Fatalf("输入 %q 应报错", raw)
		}
	}
}

// TestImportNativeFileEndToEnd 端到端：上传 auths 文件 → 落盘 → LoadDir 读回一致。
//
// 这条是「免登录迁移」的验收判据：读回来的 refresh token 与 device token 必须还在，
// 否则账号能显示但下一次刷新/请求就会掉。
func TestImportNativeFileEndToEnd(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", Pool: pool.New(""), AuthDir: dir})

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "workbuddy-u-1001.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(nativeAuthJSON)); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	p.importCockpit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	if out.Imported != 1 || out.Skipped != 0 {
		t.Fatalf("导入结果 imported=%d skipped=%d errors=%v", out.Imported, out.Skipped, out.Errors)
	}

	// 落盘文件名沿用 workbuddy-<uid>.json（池启动扫描的就是这个名字）
	path := filepath.Join(dir, "workbuddy-u-1001.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("凭据没落盘: %v", err)
	}
	// 回读：这是「免登录」成立与否的关键
	loaded, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("应读回 1 个账号，实际 %d", len(loaded))
	}
	got := loaded[0]
	if got.UID != "u-1001" || got.RefreshToken != "RT-abc" {
		t.Fatalf("回读凭据不一致: %+v", got)
	}
	if got.DeviceToken != "dt-xyz" {
		t.Fatalf("device_token 没落到磁盘（下次请求会当新设备）: %+v", got)
	}
}
