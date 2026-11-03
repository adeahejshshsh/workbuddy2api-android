package pool

// 积分保底按域覆盖（issue #147）：国内池留 1000 分养号、国际池留 350 分接低流量，
// 此前只能共用一个阈值。这里钉住取值语义 —— 真正的选择行为由 floorBlocked* 消费它。

import "testing"

func TestCreditFloorForRealm(t *testing.T) {
	p := New("")
	p.SetCreditFloor(1000)
	p.SetCreditFloorByRealm(map[string]int64{"global": 350, "cn": 0})

	cases := []struct {
		realm string
		want  int64
		why   string
	}{
		{"cn", 0, "显式写 0 = 该域关闭保底（不是「没配」）"},
		{"global", 350, "按域覆盖生效"},
		{"", 0, "空 realm 归一化为 cn"},
		{"other", 1000, "非法/未写的域回落全局阈值"},
	}
	for _, c := range cases {
		if got := p.creditFloorFor(c.realm); got != c.want {
			t.Errorf("creditFloorFor(%q) = %d, want %d（%s）", c.realm, got, c.want, c.why)
		}
	}

	// nil 表 = 全部回落全局
	p.SetCreditFloorByRealm(nil)
	if got := p.creditFloorFor("global"); got != 1000 {
		t.Errorf("清空覆盖表后 global = %d, want 1000（回落全局）", got)
	}
}
