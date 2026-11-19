// images_charges_cap_test.go W1-07 回归：出图实扣分档表必须有界。
//
// 键里带 size/quality（客户端请求体可控），表只写不删 —— 随机尺寸/画质可以把它
// 撑到任意大，面板每次读还要全表遍历 + 排序。
package upstream

import (
	"fmt"
	"testing"
)

func TestRecordImageChargeBounded(t *testing.T) {
	c := &Client{}

	// 先养一个「真实档位」：反复出图，样本数高。
	for i := 0; i < 10; i++ {
		c.RecordImageCharge("cn", "gpt-image", "1024x1024", "high", 1, 0, 0)
	}
	// 再用不重复的尺寸/画质刷爆它。
	for i := 0; i < imageChargeMaxBuckets*2; i++ {
		c.RecordImageCharge("cn", "gpt-image", fmt.Sprintf("%dx%d", i+1, i+2), fmt.Sprintf("q%d", i), 1, 0, 0)
	}

	c.imageChargeMu.Lock()
	n := len(c.imageCharges)
	c.imageChargeMu.Unlock()
	if n > imageChargeMaxBuckets {
		t.Fatalf("分档表无界：%d > %d", n, imageChargeMaxBuckets)
	}

	// 热档位不能被一次性桶挤掉（淘汰按样本数最少优先）。
	found := false
	for _, agg := range c.ImageCharges("cn", "gpt-image") {
		if agg.Size == "1024x1024" && agg.Quality == "high" {
			found = true
			if agg.Samples != 10 {
				t.Fatalf("热档位样本数被改写：%d", agg.Samples)
			}
		}
	}
	if !found {
		t.Fatal("热档位被一次性桶淘汰了（淘汰策略应按样本数最少优先）")
	}
}
