package server

// alert_source.go 把 *Handler 手上的两个观测点（池健康、IP 级 WAF 拦截）接给
// internal/alert。告警监视器只认 alert.Source 这个两方法接口，不认识 pool/Handler，
// 所以在边界上做一次适配——两边都不必为对方改形状。

import "github.com/linguo2625469/workbuddy2api-panel/internal/alert"

// AlertSource 返回告警监视器要的观测源；未接池时返回 nil（监视器会静默跳过）。
func (h *Handler) AlertSource() alert.Source {
	if h == nil || h.cfg.Pool == nil {
		return nil
	}
	return handlerAlertSource{h: h}
}

type handlerAlertSource struct{ h *Handler }

func (s handlerAlertSource) Health(realm string) alert.Health {
	rh := s.h.cfg.Pool.RealmHealth(realm)
	return alert.Health{
		Total:   rh.Total,
		Healthy: rh.Healthy,
		Cooling: rh.Cooling,
		// Disabled 与 /metrics 同口径合并 d+p：对告警接收方来说「禁用」和「暂停选号」
		// 都是「这个号不接单」，拆成两列只会让看板少算一半。
		Disabled: rh.Disabled + rh.Paused,
		Breaker:  rh.Breaker,
		Degraded: rh.Degraded,
		InFlight: rh.InFlightFull,
	}
}

// WAFActive 报告 IP 级 WAF 拦截当前是否激活（见 wafip.go）。
func (s handlerAlertSource) WAFActive() bool { return s.h.wafIP.active() }

// 编译期确认适配器满足接口（改 alert.Source 时这里先红）。
var _ alert.Source = handlerAlertSource{}
