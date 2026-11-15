package session

import (
	"strings"
	"testing"
)

// 会话级与轮级聚合 ID 不能撞（审查 I1）：同盐同分隔符时，客户端把 conversationId
// 取成轮键形态（`u0:xxx`）就会让两类 ID 逐字节相同，上游后台把「一个对话」与
// 「一个轮」并成同一 aggregate。
func TestSessionAndTurnIDsNeverCollide(t *testing.T) {
	// 构造一对「裸拼接会撞」的输入：sess 侧 key 与 turn 侧 turnKey 相同。
	k := "u0:hello"
	a := RequestIDForKey(k)
	b := TurnRequestID(k)
	if a == b {
		t.Fatalf("会话级与轮级 ID 撞了（都等于 %s）", a)
	}
}

// 熵源兜底也要保持 UUID 形态（审查 I3）：文档承诺 36 字符带横线。
func TestNewConversationIDShapeAlwaysUUID(t *testing.T) {
	for i := 0; i < 20; i++ {
		id := NewConversationID()
		if len(id) != 36 {
			t.Fatalf("长度 %d（want 36）：%q", len(id), id)
		}
		if strings.Count(id, "-") != 4 {
			t.Fatalf("横线数 %d（want 4）：%q", strings.Count(id, "-"), id)
		}
	}
}

// 出站头的 conversationId 必须过白名单（审查 I6）：含 CR/LF 会让 Header 写入失败
// 或请求被拒（表现为 500 而不是 400）。
func TestResolveConversationIDRejectsUnsafeHeaderValues(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"conversation_id":"ok-1_2:3.x"}`, "ok-1_2:3.x"},
		{`{"conversation_id":"bad\r\nX-Evil: 1"}`, ""},
		{`{"conversation_id":"has space"}`, ""},
		{`{"metadata":{"conversationId":"meta-ok"}}`, "meta-ok"},
		{`{"conversation_id":"` + strings.Repeat("a", 129) + `"}`, ""},
	}
	for _, c := range cases {
		if got := ResolveConversationID([]byte(c.body)); got != c.want {
			t.Errorf("body=%s → %q want %q", c.body, got, c.want)
		}
	}
}

// validConversationID 的白名单边界。
func TestValidConversationIDWhitelist(t *testing.T) {
	ok := []string{"a", "A-1_b.c:d", strings.Repeat("x", 128)}
	bad := []string{"", " ", "a b", "a\nb", "a/b", "中文", strings.Repeat("x", 129)}
	for _, v := range ok {
		if !validConversationID(v) {
			t.Errorf("%q 应合法", v)
		}
	}
	for _, v := range bad {
		if validConversationID(v) {
			t.Errorf("%q 应被拒", v)
		}
	}
}

// StopGC 之后不能再往表里写（审查 S9）：关停顺序若错（先 StopGC 后仍有在途请求），
// 表会继续被写但再没人回收 —— 无界增长且无人管。
func TestResolveAfterStopGCDoesNotGrow(t *testing.T) {
	r := New(Config{})
	r.cfg.Available = func() []string { return []string{"u1", "u2"} }
	r.StartGC()
	r.StopGC()

	before := 0
	r.mu.RLock()
	before = len(r.entries)
	r.mu.RUnlock()

	for i := 0; i < 100; i++ {
		uid, ok := r.Resolve("key-" + string(rune('a'+i%26)))
		if !ok || uid == "" {
			t.Fatalf("关停后仍应能返回一个可用 uid（无状态轮换），got %q ok=%v", uid, ok)
		}
	}
	r.mu.RLock()
	after := len(r.entries)
	r.mu.RUnlock()
	if after != before {
		t.Fatalf("StopGC 之后表仍在增长：%d → %d", before, after)
	}
}
