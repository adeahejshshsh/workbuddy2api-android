package server

// W2-07 回归：客户端可控的 model 名进日志前必须剥控制字符并限长。
//
// 一个 "\n" 就能在日志里伪造一行（伪造 ERR/审计行）；几十 MB 的 model 名
// 能把单行日志刷爆。

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLogModelStripsControlCharacters(t *testing.T) {
	in := "glm-5.2\n2026-11-19 ERR: [server] forged line\r\tend"
	got := logModel(in)
	for _, bad := range []string{"\n", "\r", "\t"} {
		if strings.Contains(got, bad) {
			t.Fatalf("日志模型名仍含控制字符 %q：%q", bad, got)
		}
	}
	if !strings.Contains(got, "forged line") {
		t.Fatalf("可打印内容应保留（只是不能换行）：%q", got)
	}
}

func TestLogModelBoundsLength(t *testing.T) {
	in := "m" + strings.Repeat("x", 10_000)
	got := logModel(in)
	if n := utf8.RuneCountInString(got); n > logModelMaxRunes+1 {
		t.Fatalf("长度 %d 超过上限 %d（+省略号）", n, logModelMaxRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长应带省略号：%q", got[len(got)-10:])
	}
}

func TestLogModelKeepsNormalName(t *testing.T) {
	for _, in := range []string{"glm-5.2", "cn:deepseek-v4.1-flash", "gpt-image-2.5-sunburst"} {
		if got := logModel(in); got != in {
			t.Fatalf("正常模型名不该被改写：%q -> %q", in, got)
		}
	}
}
