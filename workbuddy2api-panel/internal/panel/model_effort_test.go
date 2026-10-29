package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSetModelEffortWritesDomainKey 行内「网关档位」写 thinking.model_defaults 的
// 「域:模型」精确键：既有裸名覆盖（两域共用）与别的域条目必须原样保留，模型名统一小写。
func TestSetModelEffortWritesDomainKey(t *testing.T) {
	var saved map[string]any
	pn := New(Config{
		Version: "test", APIKey: "k",
		LoadConfig: func() (any, error) {
			return map[string]any{
				"thinking": map[string]any{
					"client_priority": true,
					"model_defaults": map[string]any{
						"glm-5.2":            "high",
						"global:deepseek-v4": "xhigh",
					},
				},
			}, nil
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatalf("saved body is not JSON: %v (%s)", err, raw)
			}
			return nil, nil
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/model_effort",
		strings.NewReader(`{"id":"cn:GLM-5.2","effort":"Max"}`))
	pn.setModelEffort(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	thinking, _ := saved["thinking"].(map[string]any)
	got, _ := thinking["model_defaults"].(map[string]any)
	if got["cn:glm-5.2"] != "max" {
		t.Errorf("精确键应小写写入 max：%v", got)
	}
	if got["glm-5.2"] != "high" || got["global:deepseek-v4"] != "xhigh" {
		t.Errorf("既有条目被改写：%v", got)
	}
	if _, ok := saved["models"]; ok {
		t.Errorf("不应顺带提交其它配置节：%v", saved)
	}
}

// TestSetModelEffortAcceptsOff off 是面板下拉里与 supportedEfforts 同列的选项：
// 表示该模型网关侧固定关闭思考，允许写入（是否生效由 scope 与客户端意图决定）。
func TestSetModelEffortAcceptsOff(t *testing.T) {
	var saved map[string]any
	pn := New(Config{
		Version: "test", APIKey: "k",
		LoadConfig: func() (any, error) {
			return map[string]any{"thinking": map[string]any{"model_defaults": map[string]any{"cn:glm-5.2": "high"}}}, nil
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatalf("saved body is not JSON: %v (%s)", err, raw)
			}
			return nil, nil
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/model_effort",
		strings.NewReader(`{"id":"cn:glm-5.2","effort":"off"}`))
	pn.setModelEffort(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	thinking, _ := saved["thinking"].(map[string]any)
	got, _ := thinking["model_defaults"].(map[string]any)
	if got["cn:glm-5.2"] != "off" {
		t.Errorf("off 应原样写入：%v", got)
	}
}

// TestSetModelEffortRejectsInvalid 非法输入一律 400 且不落盘：id 必须带域前缀，
// effort 只能是合法档位或 off。
func TestSetModelEffortRejectsInvalid(t *testing.T) {
	cases := []struct{ name, body string }{
		{"无域前缀", `{"id":"glm-5.2","effort":"max"}`},
		{"未知域", `{"id":"eu:glm-5.2","effort":"max"}`},
		{"空模型名", `{"id":"cn:","effort":"max"}`},
		{"乱填档位", `{"id":"cn:glm-5.2","effort":"ultra"}`},
		{"空档位", `{"id":"cn:glm-5.2","effort":""}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			saved := false
			pn := New(Config{
				Version: "test", APIKey: "k",
				LoadConfig: func() (any, error) { return map[string]any{}, nil },
				SaveConfig: func([]byte) ([]string, error) { saved = true; return nil, nil },
			})
			rec := httptest.NewRecorder()
			pn.setModelEffort(rec, httptest.NewRequest("POST", "/panel/api/model_effort", strings.NewReader(c.body)))
			if rec.Code != 400 {
				t.Errorf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
			}
			if saved {
				t.Error("非法输入不应触发保存")
			}
		})
	}
}
