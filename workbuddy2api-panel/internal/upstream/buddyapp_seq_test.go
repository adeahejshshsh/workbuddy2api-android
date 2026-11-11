package upstream

import "testing"

// 服务端对 /v2/report 的事件做格式校验，buddyapp_enter_click 的 isFirstPage
// 只接受布尔值：传字符串 "1" 时整条事件被丢弃，响应仍是 2xx：
//
//	{"code":10001,"msg":"partial success: kept=4 events, dropped=1 events by format validation",
//	 "data":{"dropped":1,"invalidIndices":[2],"kept":4}}
//
// invalidIndices=[2] 正好是五连事件里的第三条（discover → show → enter），
// 任务判据吃不到这条，Buddy_App / Buddy_App_QQ 就永远停在 0/1。
// 这个用例钉住类型：改回字符串会直接红。
func TestDesktopBuddyAppSequenceIsFirstPageIsBool(t *testing.T) {
	seq := DesktopBuddyAppSequence("buddy-1", "测试 Buddy")
	if len(seq) != 5 {
		t.Fatalf("五连事件应为 5 条，实际 %d", len(seq))
	}
	var enter DesktopEvent
	for _, ev := range seq {
		if code, _ := ev["eventCode"].(string); code == "buddyapp_enter_click" {
			enter = ev
			break
		}
	}
	if enter == nil {
		t.Fatal("事件链里没有 buddyapp_enter_click")
	}
	v, ok := enter["isFirstPage"]
	if !ok {
		t.Fatal("buddyapp_enter_click 缺少 isFirstPage")
	}
	if _, isBool := v.(bool); !isBool {
		t.Fatalf("isFirstPage 必须是 bool，实际 %T(%v)：字符串会被上游格式校验丢弃", v, v)
	}
}
