package upstream

import (
	"strings"
	"testing"
)

// DSH 身份句品牌词必须被改写：上游按逐字匹配把 "DeepSeek Harness" 当成
// 「非官方渠道调用」整单拦截（400 11‑-128 unapproved channel）。
// 依据 misakano7545/workbuddy2api-panel `4f60a4c6` 的实测结论。
func TestDeepSeekHarnessBrandRewritten(t *testing.T) {
	in := "You are an AI agent powered by DeepSeek Harness.\n\nCheck the exit code."
	if !hasFingerprint(in) {
		t.Fatal("只带品牌词的文本没进净化（特征表缺 DeepSeek Harness）")
	}
	out := sanitizeText(in)
	if strings.Contains(out, "DeepSeek Harness") {
		t.Fatalf("品牌词没被改写: %q", out)
	}
	if !strings.Contains(out, "DeepSeekRuntime") {
		t.Fatalf("改写结果不含替代词: %q", out)
	}
	// 幂等：再跑一次不再变化（替代词不含原短语）
	if again := sanitizeText(out); again != out {
		t.Fatalf("非幂等: %q -> %q", out, again)
	}
	// 正文中多次出现也要全部改写（按短语全局替换，不是只认开头）
	multi := "deep copy: DeepSeek Harness again and DeepSeek Harness once more"
	if got := sanitizeText(multi); strings.Contains(got, "DeepSeek Harness") {
		t.Fatalf("正文里的品牌词没全部改写: %q", got)
	}
}
