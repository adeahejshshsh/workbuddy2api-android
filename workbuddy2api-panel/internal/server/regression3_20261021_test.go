package server

import "testing"

// BUG-1：hasImagePart 不能因为「有纯文本消息」就整体失效。
func TestHasImagePartMixedMessages(t *testing.T) {
	// 系统提示是字符串、用户消息带图：真实请求的典型形态
	body := []byte(`{"messages":[
		{"role":"system","content":"you are helpful"},
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"x"}}]}
	]}`)
	if !hasImagePart(body) {
		t.Fatal("混合消息里的图片没被识别（旧结构会因 content 是字符串整体报错）")
	}
	// 纯文本：必须为 false
	if hasImagePart([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)) {
		t.Fatal("纯文本误判为带图")
	}
}
