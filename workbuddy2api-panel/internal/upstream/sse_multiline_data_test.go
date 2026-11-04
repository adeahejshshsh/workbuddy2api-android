package upstream

// SSE 规范允许一个事件的多行 data: 用 \n 拼成一条消息（JSON 美化输出/长行折行时
// 上游会这么发）。此前每读到一行 data: 就当独立帧处理 —— 一条多行 JSON 被拆成 N 个
// 碎片，每个都不是合法 JSON，writeFrame 走「非 JSON 原样降级」分支逐条写出，
// 客户端按帧解析全部失败。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamHintJoinsMultiLineData(t *testing.T) {
	// 一个事件，JSON 被拆成三行 data:。
	raw := "data: {\"id\":\"c1\",\n" +
		"data: \"choices\":[{\"index\":0,\"delta\":{\"content\":\"多行\"}}]}\n" +
		"\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()

	// 拼回来的那一帧必须是完整 JSON（能解析出 content）。
	if !strings.Contains(body, `"content":"多行"`) {
		t.Fatalf("多行 data 未拼成一条完整帧：%q", body)
	}
	// 不该出现被拆碎的半截 JSON 帧。
	if strings.Contains(body, `data: {"id":"c1",`+"\n\n") {
		t.Fatalf("多行 data 被拆成独立帧（客户端解析必失败）：%q", body)
	}
}

// 单行 JSON（绝大多数上游）走的是「立即写出」快路，不该被我的缓冲逻辑改动。
// 注意帧本身会被 normalizeFrame 按白名单重建（既有行为），所以这里只断言内容与
// 帧数，不断言逐字节相等。
func TestStreamHintSingleLineDataUnchanged(t *testing.T) {
	raw := `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("单行帧内容丢失：%q", body)
	}
	// 只应有 1 个 data 帧（外加 [DONE]）—— 缓冲逻辑不得把单行拆成多个事件。
	if n := strings.Count(body, "data: "); n != 2 {
		t.Fatalf("data 帧数=%d，应为 2（1 内容帧 + [DONE]）：%q", n, body)
	}
}
