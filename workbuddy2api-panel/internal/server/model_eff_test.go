package server

// 降级后的实际模型要落到请求记录（reqlog.Event.ModelEff）：面板靠它把
// 「请求 A、实际用 B 成功」这件事显示出来。不记这一位，6004 降级在事后完全不可见——
// 面板显示原模型成功，运维根本不知道流量被改道过。

import (
	"testing"
	"time"
)

func TestEventCarriesModelEff(t *testing.T) {
	tr := &requestTrace{start: time.Now(), path: "/v1/chat/completions"}
	tr.stat = &chatStat{model: "deepseek-v4.1-flash", modelEff: "deepseek-v4-flash"}

	e := tr.event(200)
	if e.Model != "deepseek-v4.1-flash" {
		t.Fatalf("Model=%q want 请求模型", e.Model)
	}
	if e.ModelEff != "deepseek-v4-flash" {
		t.Fatalf("ModelEff=%q want 实际模型（降级不可丢）", e.ModelEff)
	}
}

func TestEventModelEffEmptyWhenNoFallback(t *testing.T) {
	tr := &requestTrace{start: time.Now()}
	tr.stat = &chatStat{model: "deepseek-v4.1-flash"}
	if got := tr.event(200).ModelEff; got != "" {
		t.Fatalf("未降级时 ModelEff 应为空，got %q", got)
	}
}
