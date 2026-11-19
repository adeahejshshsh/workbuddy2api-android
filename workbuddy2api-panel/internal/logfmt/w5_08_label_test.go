package logfmt

import (
	"strings"
	"testing"
)

// W5-08 回归：昵称是外部可控文本，Label 必须剥掉换行/回车/制表/控制字符并限长，
// 否则日志行可被伪造（117 处调用点不可能逐个转义）。
func TestW5_08LabelSanitizesNickname(t *testing.T) {
	got := Label("u1", "bad\nname\r\x1b[31m\tX")
	for _, bad := range []string{"\n", "\r", "\x1b", "\t"} {
		if strings.Contains(got, bad) {
			t.Fatalf("Label 输出仍含控制字符 %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "(u1)") {
		t.Fatalf("uid 标签丢失: %q", got)
	}

	// 长度上限：昵称不能把每条日志撑爆（48 字节上限 + "(u1)"）。
	long := Label("u1", strings.Repeat("阿", 200))
	if len(long) > maxLabelNickBytes+len("(u1)") {
		t.Fatalf("昵称未限长：%d 字节（上限 %d）", len(long), maxLabelNickBytes)
	}
	// 正常中文昵称原样保留（不能把正常的非 ASCII 文本误杀）。
	if got := Label("u1", "示例昵称甲"); got != "示例昵称甲(u1)" {
		t.Fatalf("正常昵称被改写: %q", got)
	}
}
