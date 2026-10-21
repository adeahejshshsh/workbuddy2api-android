package upstream

import (
	"strings"
	"testing"
)

// BUG-02：脱敏不许删掉正文首尾空白（缩进/代码块结构会被静默破坏）。
func TestSanitizeKeepsLeadingTrailingWhitespace(t *testing.T) {
	in := "  x-anthropic-billing-hdr  \n    indented code line\n"
	out := sanitizeText(in)
	if out == in {
		return // 没命中指纹时原样返回，也算正确
	}
	if !strings.HasPrefix(out, " ") {
		t.Fatalf("行首空白被删了：%q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("行尾空白被删了：%q", out)
	}
}

// BUG-03：反探测数字串只改写「独立出现」的形态，不许篡改 base64/ID 里的同形子串。
func TestSanitizeCodeOnlyStandalone(t *testing.T) {
	// 独立出现 → 必须改写
	if out := sanitizeText("upstream code=11-128 here"); strings.Contains(out, "11-128") {
		t.Fatalf("独立出现的错误码没被改写：%q", out)
	}
	// 嵌在更长的 token 里 → 不许动（否则 base64/ID 被篡改）
	for _, in := range []string{
		"payload=abc11-128def",
		"id=1111-1289",
		"blob=x11-128y",
	} {
		if out := sanitizeText(in); out != in {
			t.Fatalf("长 token 内的子串被误改：%q -> %q", in, out)
		}
	}
}

// BUG-04：显式空 stream_options 也要补 include_usage。
func TestPrepareBodyFillsEmptyStreamOptions(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","stream":true,"stream_options":{},"messages":[]}`)
	out := PrepareBodyOpt(body, false)
	if !strings.Contains(string(out), `"include_usage":true`) {
		t.Fatalf("空 stream_options 没补 include_usage：%s", out)
	}
	// 显式给 false 的照旧不动
	body2 := []byte(`{"model":"glm-5.2","stream":true,"stream_options":{"include_usage":false},"messages":[]}`)
	out2 := PrepareBodyOpt(body2, false)
	if strings.Contains(string(out2), `"include_usage":true`) {
		t.Fatalf("显式 false 被覆盖了：%s", out2)
	}
}
