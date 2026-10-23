package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsIncludesImageModels 面板「模型与档位」必须列出出图模型：
// 它们被目录按 tag（text-to-image）从对话列表剔掉，若不单列，面板上就看不到
// hunyuan-image-alpha / gpt-image-2.5-sunburst —— 用户不知道有哪些出图模型可调。

// fakeRoundTripper 测试用 http.RoundTripper（本包内没有现成的，server 包那个不跨包）。
type fakeRoundTripper func(*http.Request) (*http.Response, error)

func (f fakeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestPanelModelsAttachesBusyLevels 面板「模型与档位」附带上游繁忙度（/v2/chat/busy，只读）：
// 按域问对应账号，只附到该域的行上；上游没返回的模型不填字段（不编造）。
func TestPanelModelsAttachesBusyLevels(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			body := `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"}]}}`
			switch {
			case strings.HasSuffix(r.URL.Path, "/v2/chat/busy"):
				body = `{"code":0,"msg":"OK","data":[{"model":"glm-5.2","busy_level":"congested"}]}`
			case strings.HasSuffix(r.URL.Path, "/v3/config"):
				body = `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM-5.2"},` +
					`{"id":"glm-5.3-flash","name":"GLM-5.3 Flash"}]}}`
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"})
	p.SetCredits("c1", 1000, 0)
	up.FetchModels(p.AuthByUID("c1"))

	pn := New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	pn.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, m := range resp.Models {
		if v, ok := m["busy"]; ok {
			seen[m["id"].(string)] = v.(string)
		}
	}
	if seen["cn:glm-5.2"] != "congested" {
		t.Fatalf("cn:glm-5.2 繁忙度未附上: %v（全部 entry=%v）", seen, resp.Models)
	}
	if _, ok := seen["cn:glm-5.3-flash"]; ok {
		t.Fatalf("上游未返回的模型不该凭空有繁忙度: %v", seen)
	}
}
