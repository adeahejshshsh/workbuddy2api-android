package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// busyStub 记录落点与请求体，按批回对应结果。
type busyStub struct {
	paths  []string
	hosts  []string
	bodies []string
	levels map[string]string // model → busy_level（其余回 normal）
}

func (b *busyStub) client() *Client {
	return &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			b.paths = append(b.paths, r.URL.Path)
			b.hosts = append(b.hosts, r.URL.Host)
			b.bodies = append(b.bodies, string(raw))
			var req struct {
				Models []string `json:"models"`
			}
			_ = json.Unmarshal(raw, &req)
			rows := make([]string, 0, len(req.Models))
			for _, m := range req.Models {
				lv := "normal"
				if v, ok := b.levels[m]; ok {
					lv = v
				}
				rows = append(rows, `{"model":"`+m+`","busy_level":"`+lv+`"}`)
			}
			body := `{"code":0,"msg":"OK","data":[` + strings.Join(rows, ",") + `]}`
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   io.NopCloser(strings.NewReader(body))}, nil
		})},
		ChatBaseCN:    "https://cn.example",
		BillingBaseCN: "https://cn.example",
	}
}

// TestModelBusyPathBodyAndParse 落点与体：必须 /v2/chat/busy（chat base），"models" 数组。
func TestModelBusyPathBodyAndParse(t *testing.T) {
	b := &busyStub{levels: map[string]string{"glm-5.3-flash": "idle", "deepseek-v4-pro": "congested"}}
	got, err := b.client().ModelBusy(&auth.Auth{UID: "u1", AccessToken: "at"}, []string{"glm-5.3-flash", "deepseek-v4-pro"})
	if err != nil {
		t.Fatal(err)
	}
	if b.paths[0] != "/v2/chat/busy" || b.hosts[0] != "cn.example" {
		t.Fatalf("落点 %s%s want /v2/chat/busy @ cn.example", b.hosts[0], b.paths[0])
	}
	var sent struct {
		Models []string `json:"models"`
	}
	_ = json.Unmarshal([]byte(b.bodies[0]), &sent)
	if len(sent.Models) != 2 {
		t.Fatalf("请求体 models=%v", sent.Models)
	}
	if got["glm-5.3-flash"] != "idle" || got["deepseek-v4-pro"] != "congested" {
		t.Fatalf("解析不符: %v", got)
	}
}

// TestModelBusyBatchesOver50 单次上限 50：超出的模型必须分批（超限整批被上游拒）。
func TestModelBusyBatchesOver50(t *testing.T) {
	var models []string
	for i := 0; i < 120; i++ {
		models = append(models, "m"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	b := &busyStub{levels: map[string]string{}}
	got, err := b.client().ModelBusy(&auth.Auth{UID: "u1", AccessToken: "at"}, models)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.paths) != 3 {
		t.Fatalf("批次=%d want 3（120 个模型 / 每批 50）", len(b.paths))
	}
	for i, body := range b.bodies {
		var sent struct {
			Models []string `json:"models"`
		}
		_ = json.Unmarshal([]byte(body), &sent)
		if len(sent.Models) > 50 {
			t.Fatalf("第 %d 批 %d 个模型，超过上游上限 50", i+1, len(sent.Models))
		}
	}
	if len(got) != 120 {
		t.Fatalf("结果数=%d want 120", len(got))
	}
}
