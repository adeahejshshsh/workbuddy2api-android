package upstream

import (
	"net/http"
	"strings"
	"testing"
)

// TestBackendUnsupported11103IsRequestLevel 11103「Backend [x] is not supported」是
// 模型侧配置问题（issue #280 真机：25 个号轮转 25 次同一句，上游自带 retry_policy
// .retry=false）——必须归请求级，换号无意义。
//
// 落 ErrClient 的后果是双重的：每个健康号白打一遍 + 各喂一笔连败，5 次达阈出池，
// 末端 503 no_healthy_account 把「换模型」报成「账号都挂了」。
func TestBackendUnsupported11103IsRequestLevel(t *testing.T) {
	body := `{"code":11103,"msg":"Backend [hunyuan-stream] is not supported","requestId":"01a1f2d8f5b2d75cc8d39a66c42193","retry_policy":{"retry":false}}`
	if got := Classify(http.StatusBadRequest, body); got != ErrModelParamInvalid {
		t.Fatalf("Classify(400, 11103) = %v, want ErrModelParamInvalid", got)
	}
	hint := GatewayHint(ErrModelParamInvalid, body, HintContext{})
	if !strings.Contains(hint, "switch model") || !strings.Contains(hint, "switching accounts will not help") {
		t.Fatalf("hint 应点名换模型且换号无用: %q", hint)
	}
	// 流式路径：首帧 error 帧里的 11103 同样按请求级识别（否则首帧否决会换号）。
	frame := `{"error":{"code":11103,"message":"Backend [hunyuan-stream] is not supported"}}`
	if got := FrameKind(frame); got != ErrModelParamInvalid {
		t.Fatalf("FrameKind(11103) = %v, want ErrModelParamInvalid", got)
	}
}
