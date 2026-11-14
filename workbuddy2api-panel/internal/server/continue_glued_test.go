package server

import (
	"io"
	"strings"
	"testing"

)

// continueReader：上游把多个 chunk 粘在一个 data 行里（issue #197）时拆成独立帧，正文照常
// 记账；粘在里面的 error 帧同样能被识别，不会被当成「无 finish 断流」去续写。
//
// 首帧否决（HeadVetoError）那半本仓没有——那是更早的上游提交带来的结构，故这里只钉
// 「拆帧后每份都记状态」这条本仓能保证的行为。
func TestContinueReaderSplitsGluedFrames(t *testing.T) {
	frame := func(txt string) string {
		return `{"id":"a","choices":[{"index":0,"delta":{"content":"` + txt + `"}}]}`
	}
	open := func(stream string) *continueReader {
		up, _ := fakeContinueUpstream(t, func(int) (int, string) { return 200, "" })
		return newTestContinueReader(t, up, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			io.NopCloser(strings.NewReader(stream)))
	}

	cont := open("data: " + frame("x") + " " + frame("y") + "\n\ndata: [DONE]\n\n")
	out, _ := io.ReadAll(cont)
	if cont.text.String() != "xy" || strings.Count(string(out), "data: {") != 2 {
		t.Fatalf("粘帧应拆成两帧且正文都记账: text=%q\n%s", cont.text.String(), out)
	}

	// 粘在首位的 error 帧：拆开后必须走 error 分支（不进正文、不被当成普通正文累积），
	// 且客户端收到的仍是合法帧序列。
	errFrame := `{"error":{"message":"busy","code":"6021"}}`
	cont = open("data: " + errFrame + " " + frame("x") + "\n\ndata: [DONE]\n\n")
	out, _ = io.ReadAll(cont)
	if strings.Contains(cont.text.String(), "busy") {
		t.Fatalf("error 帧不该进正文累积: %q", cont.text.String())
	}
	if strings.Count(string(out), "data: {") < 2 {
		t.Fatalf("拆开后应至少两帧:\n%s", out)
	}
}
