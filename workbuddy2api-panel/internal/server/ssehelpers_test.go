package server

import (
	"encoding/json"
	"strings"
)

// SSE 断言辅助：从响应体里取出事件类型序列 / 判断某事件是否出现。
//
// 这两个 helper 在上游是写在 responses_test.go 里的；本仓库先移植 messages 适配器，
// 故单独成文件。移植 responses_test.go 时**不要**再声明一份（会重复定义）。

func sseTypes(body string) []string {
	var types []string
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
				continue
			}
			if t, ok := m["type"].(string); ok {
				types = append(types, t)
			}
		}
	}
	return types
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
