package server

import (
	"encoding/json"
	"testing"
)

// BUG-03：非流式 n>1 必须钳到 1（否则多条候选会被拼成一段正文）。
func TestClampNToOne(t *testing.T) {
	in := `{"model":"glm-5.2","n":3,"messages":[]}`
	out := clampNToOne([]byte(in))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%s)", err, out)
	}
	if n, _ := obj["n"].(float64); n != 1 {
		t.Fatalf("n 没被钳到 1：%v", obj["n"])
	}
	// 没有 n / n<=1 / 非法 body：原样返回（不许平白改写请求）
	for _, keep := range []string{`{"model":"x"}`, `{"n":1}`, `{"n":0}`, `not-json`} {
		if got := string(clampNToOne([]byte(keep))); got != keep {
			t.Fatalf("不该改写的输入被改了：%q -> %q", keep, got)
		}
	}
}
