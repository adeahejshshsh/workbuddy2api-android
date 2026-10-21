package upstream

import (
	"net/http"
	"testing"
)

// 14003「模型繁忙」是模型级、间歇信号：号是好的，只停这个模型
// （misakano7545 `f728859a` / `c083a22f`）。
func TestClassifyModelBusy(t *testing.T) {
	body := `{"code":14003,"msg":"model busy","displayMsg":"模型繁忙，请换模型或稍后重试"}`
	if got := Classify(http.StatusBadRequest, body); got != ErrModelBusy {
		t.Fatalf("14003 判成 %v，期望 ErrModelBusy", got)
	}
	if got := Classify(http.StatusOK, `{"code":14003,"msg":"model busy"}`); got != ErrModelBusy {
		t.Fatalf("200 信封里的 14003 判成 %v，期望 ErrModelBusy", got)
	}
	// 非繁忙类错误不受影响
	if got := Classify(http.StatusBadRequest, `{"code":11101,"msg":"bad params"}`); got == ErrModelBusy {
		t.Fatal("11101 不该判成 model_busy")
	}
}
