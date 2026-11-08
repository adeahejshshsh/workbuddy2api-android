package reqlog

// 请求记录里的「降级后实际模型」（model_eff）：筛选必须同时匹配请求模型与实际模型，
// 否则「按 v4.1 搜」会漏掉那些实际跑在 v4-flash 上的成功请求 —— 恰恰是排查降级时
// 最想看的那批。

import "testing"

func TestFilterMatchesModelEff(t *testing.T) {
	ev := Event{Model: "deepseek-v4.1-flash", ModelEff: "deepseek-v4-flash"}

	if !(Filter{Model: "v4.1"}).match(ev) {
		t.Fatal("按请求模型应命中")
	}
	if !(Filter{Model: "v4-flash"}).match(ev) {
		t.Fatal("按实际模型（model_eff）也应命中")
	}
	if (Filter{Model: "glm"}).match(ev) {
		t.Fatal("无关关键字不该命中")
	}
}

func TestFilterModelEffEmptyIgnored(t *testing.T) {
	// 未降级的记录 model_eff 为空：按实际模型搜不该命中它。
	ev := Event{Model: "deepseek-v4.1-flash"}
	if (Filter{Model: "v4-flash"}).match(ev) {
		t.Fatal("空 model_eff 不该匹配任何实际模型关键字")
	}
	if !(Filter{Model: "v4.1"}).match(ev) {
		t.Fatal("按请求模型仍应命中")
	}
}
