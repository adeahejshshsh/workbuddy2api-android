package upstream

// HTTP 200 内嵌 {code,msg} 失败帧（上游 2026-10-27 `8550aac` 的基础识别）。
//
// 上游在账号被判风控（11140「账号存在异常行为」）或渠道排队满（6021）时会先回 200、
// 再在流里塞一帧顶层 {"code":<非0>,"msg":"…"}（无 choices、无 error）。此前这种帧
// 被当普通 chunk 走白名单重建 —— code/msg 被剥掉，客户端只看到「干净地停止」，
// 网关也不分类：一个真被封的账号静默地继续参与选号。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamEmbeddedErrorFrameSurfaced(t *testing.T) {
	raw := "data: {\"code\":11140,\"msg\":\"账号存在异常行为\",\"requestId\":\"rq-ban-1\"}\n\n" +
		"data: [DONE]\n\n"
	var observed []string
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(raw), nil, WithErrorFrameObserver(func(p string) {
		observed = append(observed, p)
	}))
	if err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()
	// ① 必须是 error 帧，且带 code / message / requestId —— 客户端据此判失败并上报。
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("内嵌失败帧没被改写成 error 帧: %s", body)
	}
	for _, want := range []string{"11140", "账号存在异常行为", "rq-ban-1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("error 帧丢了 %q: %s", want, body)
		}
	}
	// ② 观察者必须收到（账号处置要靠它分类：11140 → ErrAccountFault/封禁口径）。
	if len(observed) != 1 || !strings.Contains(observed[0], "11140") {
		t.Fatalf("onErrorFrame 未收到该帧: %v", observed)
	}
	// ③ 不能被当成空流：客户端已经收到一帧带原因的失败。
	if strings.Contains(body, "empty upstream stream") {
		t.Fatalf("不该补写空流错误帧: %s", body)
	}
}

// 反向保护：带 choices 的正常帧、带 error 的标准错误帧都不能被这个分支吃掉。
func TestStreamErrorFrameNegative(t *testing.T) {
	cases := map[string]string{
		"带 choices": `{"choices":[{"delta":{"content":"hi"}}]}`,
		"标准 error":  `{"error":{"code":11140,"message":"x"}}`,
		"code 为 0":  `{"code":0,"msg":"ok"}`,
		"没有 code":   `{"msg":"x"}`,
	}
	for name, payload := range cases {
		var obj map[string]any
		if err := jsonUnmarshal(payload, &obj); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, _, ok := StreamErrorFrame(obj); ok {
			t.Errorf("%s：不该判成流内失败帧", name)
		}
	}
}

// jsonUnmarshal 薄封装，避免为几个用例在测试里散落 json 包引用。
func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// 非流式路径同样不能把内嵌失败帧合成为「假成功」。
//
// 这里不强行要求它升级成 ErrorFrame（上游 `8550aac` 只改了流式 writeFrame）——
// 但必须**不是** content="" + finish_reason=stop 的 200：那会让调用方把「账号被
// 封」当成功回合，还会 NoteSuccess 把被封号洗成健康号。
func TestAggregateEmbeddedErrorIsNotFakeSuccess(t *testing.T) {
	raw := "data: {\"code\":11140,\"msg\":\"账号存在异常行为\"}\n\n"
	out, err := Aggregate(strings.NewReader(raw))
	if err == nil {
		t.Fatalf("内嵌失败帧不该合成成功响应: %+v", out)
	}
}

// 内嵌失败帧的分类要落到「账号级故障」：否则 handler 只换号不罚号，被封的号会被
// 反复选中、每次都吃一次轮转预算，却永远不冷却也不下线。
//
// 上游 11140 的中文-only 文案（"账号存在异常行为"）此前不在 accountFaultMarkers 里，
// 会落 ErrClient —— 按上游用例夹具用到的这条文案补齐。
func TestEmbeddedErrorFrameClassifiesAsAccountFault(t *testing.T) {
	frame := streamErrorPayload(11140, "账号存在异常行为", `{"code":11140,"msg":"账号存在异常行为"}`)
	if k := FrameKind(frame); k != ErrAccountFault {
		t.Fatalf("FrameKind=%v want ErrAccountFault（否则只换号不罚号）", k)
	}
	// 英文形态同样要命中（真实报文里两者常一起出现）。
	if k := FrameKind(streamErrorPayload(11140, "request illegal", "")); k != ErrAccountFault {
		t.Fatalf("英文形态 FrameKind=%v want ErrAccountFault", k)
	}
}
