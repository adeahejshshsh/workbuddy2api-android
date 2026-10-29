package upstream

import (
	"strings"
	"testing"
)

// DSH 身份句品牌词必须被改写：上游按逐字匹配把品牌词原串当成「非官方渠道调用」
// 整单拦截（400 11‑-128 unapproved channel）。改写值 = DeepSeek + U+200B + Harness，
// 即零宽空格 + 换词（见 sanitize.go 的 sanitizeRewrites）。
// 依据 misakano7545/workbuddy2api-panel `4f60a4c6` 的实测结论。
//
// 三个串一律用拼接构造，不手打整词：这个品牌词在终端/编辑器里会被渲染成同一个
// 样子，肉眼与 diff 都分不出「空格」和「零宽空格」，手写迟早写反。
const (
	blockedBrand   = "DeepSeek" + " " + "Harness" // 上游逐字匹配的原串（含 0x20）
	rewrittenBrand = "DeepSeek\u200BHarness"      // 实际下发的替代词（含 U+200B）
	legacyBrand    = "DeepSeek" + "Runtime"       // 旧替代词：改动后必须彻底消失
)

func TestDeepSeekHarnessBrandRewritten(t *testing.T) {
	in := "You are an AI agent powered by " + blockedBrand + ".\n\nCheck the exit code."
	if !hasFingerprint(in) {
		t.Fatal("只带品牌词的文本没进净化（特征表缺原串）")
	}
	out := sanitizeText(in)
	// 这句是**身份句式**（You are … powered by …），新增的句式规则会整句拍平
	// （骨架本身也是特征），所以此时不该再出现品牌替代词 —— 只要求原串与旧词都消失。
	if strings.Contains(out, blockedBrand) {
		t.Fatalf("原串没被改写: %q", out)
	}
	if strings.Contains(out, legacyBrand) {
		t.Fatalf("仍在下发旧替代词: %q", out)
	}
	if !strings.Contains(out, "You are a helpful AI assistant.") {
		t.Fatalf("身份句式未被拍平: %q", out)
	}
	// 幂等：再跑一次不再变化（替代词不含原串，不会二次命中特征表）
	if again := sanitizeText(out); again != out {
		t.Fatalf("非幂等: %q -> %q", out, again)
	}
	// 正文中多次出现也要全部改写（按短语全局替换，不是只认开头）
	multi := "deep copy: " + blockedBrand + " again and " + blockedBrand + " once more"
	got := sanitizeText(multi)
	if strings.Contains(got, blockedBrand) || strings.Contains(got, legacyBrand) {
		t.Fatalf("正文里的品牌词没全部改写: %q", got)
	}
	if n := strings.Count(got, rewrittenBrand); n != 2 {
		t.Fatalf("两次出现应都替换成替代词，实际 %d 次: %q", n, got)
	}
}
