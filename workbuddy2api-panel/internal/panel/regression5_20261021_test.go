package panel

import "testing"

// BUG-13：任务码大小写口径必须与 taskMapByCode（ToLower 建键）一致。
func TestAutoActionForCaseInsensitive(t *testing.T) {
	if autoActionFor("Buddy_App") == nil {
		t.Fatal("精确大小写都查不到，测试前提不成立")
	}
	if got := autoActionFor("buddy_app"); got == nil {
		t.Fatal("小写形态查不到：队列扫描与 runAutoAll 会分叉")
	}
	if got := autoActionFor("  BUDDY_APP  "); got == nil {
		t.Fatal("带空格的大写形态查不到")
	}
}
