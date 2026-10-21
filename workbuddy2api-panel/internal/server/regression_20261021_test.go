package server

import (
	"strings"
	"testing"
	"time"
)

// bug 10：命中率 1%~9% 也必须告警（旧实现只要本次 hit>0 就直接 return）。
func TestCacheMissWarnsAtLowHitRate(t *testing.T) {
	low := &cacheMissSignal{everHit: map[string]bool{"m": true}, last: map[string]time.Time{}}
	low.noteCacheTokens("m", 5000, 1, 49) // 2%
	if _, ok := low.last["m"]; !ok {
		t.Fatal("2% 命中率没有触发告警（旧实现的 bug）")
	}

	high := &cacheMissSignal{everHit: map[string]bool{"m": true}, last: map[string]time.Time{}}
	high.noteCacheTokens("m", 5000, 40, 10) // 80%
	if _, ok := high.last["m"]; ok {
		t.Fatal("80% 命中率不该告警")
	}

	// 从未命中过的模型不告警（新模型/新会话的首请求天然全 miss）
	fresh := &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}
	fresh.noteCacheTokens("m", 5000, 0, 100)
	if _, ok := fresh.last["m"]; ok {
		t.Fatal("从未命中过的模型不该告警")
	}
}

// bug 11：出站请求体走 map 往返时，超过 2^53 的整数必须原样保留。
func TestRewriteModelKeepsBigInt(t *testing.T) {
	body := []byte(`{"model":"cn:glm-5.2","seed":9007199254740993,"temperature":0.70}`)
	out := string(rewriteModel(body, "glm-5.2"))
	if !strings.Contains(out, "9007199254740993") {
		t.Fatalf("大整数被改写：%s", out)
	}
	if !strings.Contains(out, `"model":"glm-5.2"`) {
		t.Fatalf("model 没被重写：%s", out)
	}
	if strings.Contains(out, "e+") || strings.Contains(out, "E+") {
		t.Fatalf("出现了科学计数法：%s", out)
	}
}
