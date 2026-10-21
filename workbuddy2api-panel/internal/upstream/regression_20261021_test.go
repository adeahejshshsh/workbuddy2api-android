package upstream

import "testing"

// bug #6：11115「prompt is too long」的大小写兜底是死条件。
//
// 原写法 `body 含 m || (m != ToLower(m) && lower 含 ToLower(m))`，而 markers 全是
// 小写 —— 第二段恒不执行，上游回 "Prompt is too long"/"PROMPT IS TOO LONG"
// 时判不出来，会落成 ErrNotFound（白冷却账号 60s）或 ErrClient（白换一次号）。
func TestClassifyPromptTooLongCaseInsensitive(t *testing.T) {
	bodies := []string{
		`{"error":{"message":"prompt is too long"}}`,
		`{"error":{"message":"Prompt is too long"}}`,
		`{"error":{"message":"PROMPT IS TOO LONG"}}`,
	}
	for _, body := range bodies {
		if got := Classify(400, body); got != ErrPromptTooLong {
			t.Errorf("body=%s 判成 %v，期望 ErrPromptTooLong", body, got)
		}
	}
}
