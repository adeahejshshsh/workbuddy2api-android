package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSecondCreditsIssue222 企业「不限量」的 1<<40 哨兵不得充当权重分母（issue #222）。
// 修前：maxCredits=1<<40 → 普通号的 credits 项被压到 0，不限量号独吞流量。
func TestSecondCreditsIssue222(t *testing.T) {
	const unlimited = int64(1) << 40 // 与 upstream.enterpriseUnlimitedRemain 同值
	p := New("")
	p.Add(&auth.Auth{UID: "ent"})
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetCredits("ent", unlimited, -1)
	p.SetCredits("a", 100, 100)
	p.SetCredits("b", 50, 50)

	cands := p.List()
	es := make([]*entry, 0, len(cands))
	for _, st := range cands {
		es = append(es, p.byUID[st.UID])
	}
	if got := secondCredits(es); got != 100 {
		t.Fatalf("secondCredits=%d want 100（第二大真实余额，不是 1<<40）", got)
	}

	now := time.Now()
	den := secondCredits(es)
	// 不限量号钳到满权重（1.0），普通号按真实比例，三者都必须是有限且可比的数。
	we := p.weightOfScaled(p.byUID["ent"], den, now, 1)
	wa := p.weightOfScaled(p.byUID["a"], den, now, 1)
	wb := p.weightOfScaled(p.byUID["b"], den, now, 1)
	// 哨兵号钳到满额（= 余额最多的正常号），绝不高于它；正常号之间仍按余额分档。
	if we > wa+0.001 {
		t.Fatalf("哨兵号不得高于满额号：ent=%v a=%v", we, wa)
	}
	if wb >= wa {
		t.Fatalf("余额少的号必须更低：a=%v b=%v", wa, wb)
	}
	if wa <= wb {
		t.Fatalf("余额多的号权重应更高：a=%v b=%v", wa, wb)
	}
	if wb <= 1 {
		t.Fatalf("普通号 credits 项被压没了：b=%v（修前就是这个症状）", wb)
	}
}

// TestSecondCreditsNormalMaxKept 正常余额差（哪怕 1e4 倍）不得被当成哨兵：
// 分母必须是真实最大值，否则「余额最多优先」被抹平。
func TestSecondCreditsNormalMaxKept(t *testing.T) {
	p := New("")
	for _, uid := range []string{"big", "small"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetCredits("big", 100000, 100000)
	p.SetCredits("small", 10, 10)
	es := []*entry{p.byUID["big"], p.byUID["small"]}
	if got := secondCredits(es); got != 100000 {
		t.Fatalf("secondCredits=%d want 100000（真实最大值，不是第二大）", got)
	}
	wBig := p.weightOfScaled(p.byUID["big"], 100000, time.Now(), 1)
	wSmall := p.weightOfScaled(p.byUID["small"], 100000, time.Now(), 1)
	if wBig <= wSmall {
		t.Fatalf("余额最多的号应更高：big=%v small=%v", wBig, wSmall)
	}
}

// TestSecondCreditsSingleCandidate 单号：非哨兵时照旧用自身余额当分母，权重为正。
func TestSecondCreditsSingleCandidate(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "only"})
	p.SetCredits("only", 999, 999)
	if got := secondCredits([]*entry{p.byUID["only"]}); got != 999 {
		t.Fatalf("secondCredits=%d want 999", got)
	}
	if w := p.weightOfScaled(p.byUID["only"], 999, time.Now(), 1); w <= 1 {
		t.Fatalf("单号权重应含 credits 项，got %v", w)
	}
	// 单号且是哨兵：分母退化为 0，credits 项为 0 但不 panic，权重仍为正。
	p.SetCredits("only", sentinelCredits, -1)
	den := secondCredits([]*entry{p.byUID["only"]})
	if den != 0 {
		t.Fatalf("单号哨兵分母=%d want 0", den)
	}
	if w := p.weightOfScaled(p.byUID["only"], den, time.Now(), 1); w <= 0 {
		t.Fatalf("权重必须为正，got %v", w)
	}
}
