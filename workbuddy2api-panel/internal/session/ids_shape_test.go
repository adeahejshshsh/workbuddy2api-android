package session

import (
	"regexp"
	"strconv"
	"testing"
	"time"
)

// 官方客户端的 ID 形态本身就是指纹（misakano7545 issue #51）：
// 消息级 32 hex 且**前 12 hex 是毫秒时间戳**；会话级是带横线的 UUID v4。
func TestNewMessageIDShape(t *testing.T) {
	before := time.Now().UnixMilli()
	id := NewMessageID()
	after := time.Now().UnixMilli()

	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("message id 不是 32 hex: %q", id)
	}
	ms, err := strconv.ParseInt(id[:12], 16, 64)
	if err != nil {
		t.Fatalf("前 12 hex 解不出时间戳: %q", id)
	}
	// 时间位必须落在本次调用的时间窗内（容忍 1s 的日志/时钟抖动）
	if ms < before-1000 || ms > after+1000 {
		t.Fatalf("时间位 %d 不在 [%d,%d] 内（说明没带时间前缀）", ms, before, after)
	}
	// 同会话内单调：后生成的 ID 时间位不小于先生成的
	prev := id
	for i := 0; i < 5; i++ {
		cur := NewMessageID()
		p, _ := strconv.ParseInt(prev[:12], 16, 64)
		c, _ := strconv.ParseInt(cur[:12], 16, 64)
		if c < p {
			t.Fatalf("时间位非单调: %d < %d", c, p)
		}
		prev = cur
	}
}

func TestNewConversationIDShape(t *testing.T) {
	id := NewConversationID()
	// UUID v4：8-4-4-4-12，第三段以 4 开头，第四段以 8/9/a/b 开头
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("conversation id 不是 UUID v4: %q", id)
	}
	if id == NewConversationID() {
		t.Fatal("两次生成不应相同")
	}
}
