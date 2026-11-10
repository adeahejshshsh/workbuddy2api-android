package panel

// GET /panel/api/accounts/{uid}/models_off —— 手动关闭的模型清单（只读）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func TestAccountModelsOffEndpoint(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	pl.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p := New(Config{Version: "test", Pool: pl})

	// u1 关掉两个模型（大小写/顺序随意，输出按模型名升序）
	if !pl.SetModelOff("u1", "deepseek-v4.1-flash", "手动") {
		t.Fatal("SetModelOff 失败")
	}
	if !pl.SetModelOff("u1", "glm-5.2", "") {
		t.Fatal("SetModelOff 失败")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/accounts/u1/models_off", nil)
	req.SetPathValue("uid", "u1")
	p.accountModelsOff(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		List []struct {
			Model string `json:"model"`
			Note  string `json:"note"`
		} `json:"list"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.List) != 2 {
		t.Fatalf("清单=%+v want 2 项", resp.List)
	}
	if resp.List[0].Model != "deepseek-v4.1-flash" || resp.List[1].Model != "glm-5.2" {
		t.Fatalf("应按模型名升序: %+v", resp.List)
	}
	if resp.List[0].Note != "手动" {
		t.Fatalf("备注丢了: %+v", resp.List[0])
	}

	// 没有关闭任何模型的账号：空数组（不是 null —— 前端直接 spread/for 会用）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/panel/api/accounts/u2/models_off", nil)
	req.SetPathValue("uid", "u2")
	p.accountModelsOff(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var empty struct {
		List []any `json:"list"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.List == nil {
		t.Fatalf("空清单应为 []，不是 null: %s", rec.Body)
	}

	// 不存在的账号 → 404
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/panel/api/accounts/nope/models_off", nil)
	req.SetPathValue("uid", "nope")
	p.accountModelsOff(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知账号应 404，got %d", rec.Code)
	}
}
