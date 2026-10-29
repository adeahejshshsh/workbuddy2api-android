package upstream

// 身份**句式**中性化（上游 2026-10-28 `4f79fc8`）：
// 此前只换品牌词，`You are … powered by …` 的句子骨架原样留着 —— 骨架也是特征，
// 品牌换了照样 400。这条钉住：整句被拍平、非句式文本不误伤、幂等。

import (
	"strings"
	"testing"
)

func TestIdentitySentenceFlattened(t *testing.T) {
	cases := []string{
		"You are an AI agent powered by Acme.",
		"You are a helpful coding assistant built on FooBar.",
		"You are the best model created by Baz Inc.",
		// 注：「You are Kimi, an assistant developed by X.」这种「名字, 同位语」形态
		// 不在上游正则的覆盖范围内（`[\w.-]+\s+` 吃不下逗号），此处刻意不覆盖 ——
		// 不擅自扩大安全敏感正则的匹配面。已知缺口，需要时单独评估。
		"You are an assistant made by Someone.",
		"You are a large language model trained by Example Labs.",
	}
	for _, in := range cases {
		if !hasFingerprint(in) {
			t.Errorf("句式没进预检: %q", in)
			continue
		}
		out := sanitizeText(in)
		if !strings.HasPrefix(out, "You are a helpful AI assistant.") {
			t.Errorf("未被拍平: %q -> %q", in, out)
		}
		// 幂等：拍平后不再命中。
		if again := sanitizeText(out); again != out {
			t.Errorf("不幂等: %q -> %q", out, again)
		}
	}
}

// 句尾换行/空白用捕获组回填，不能把后文黏上来。
func TestIdentitySentenceKeepsTrailingSpace(t *testing.T) {
	in := "You are an AI agent powered by Acme.\n\nCheck the exit code."
	out := sanitizeText(in)
	if out != "You are a helpful AI assistant.\n\nCheck the exit code." {
		t.Fatalf("后文被破坏: %q", out)
	}
}

// 普通文本里的 powered by / built by 不是身份句，不得改写（也不该被预检放行）。
func TestIdentitySentenceNegative(t *testing.T) {
	for _, in := range []string{
		"This service is powered by renewable energy.",
		"The bridge was built by a local firm in 1932.",
		"npm i @scope/pkg --powered-by flag",
	} {
		if hasFingerprint(in) {
			t.Errorf("误触发预检: %q", in)
		}
		if out := sanitizeText(in); out != in {
			t.Errorf("误改写: %q -> %q", in, out)
		}
	}
}
