package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDumpBlockedBodyBoundedAndPrunes 11128 快照：单份截断、最多留 3 份、0600。
func TestDumpBlockedBodyBoundedAndPrunes(t *testing.T) {
	dir := t.TempDir()
	h := &Handler{cfg: Config{BlockedDumpDir: dir}}
	big := strings.Repeat("x", blockedDumpMaxBytes+1024)
	for i := 0; i < 5; i++ {
		p := h.dumpBlockedBody("glm-5.2", "outbound", big, true)
		if p == "" {
			t.Fatalf("第 %d 次快照写入失败", i)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) > blockedDumpKeep {
		t.Fatalf("最多保留 %d 份，实际 %d 份", blockedDumpKeep, len(ents))
	}
	p := filepath.Join(dir, ents[len(ents)-1].Name())
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("快照必须是 0600，实际 %v", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(p)
	if len(raw) > blockedDumpMaxBytes+4096 {
		t.Errorf("单份快照未被截断：%d 字节", len(raw))
	}
	if !strings.Contains(string(raw), `"truncated": true`) {
		t.Errorf("截断时必须标 truncated=true")
	}
	// kind 是排障的第一手信息：入站快照里带着脱敏前的指纹，出站快照才是真正发出去
	// 的那份。没有这张标签，就会重演「快照里有指纹、上游照样 400」的误判。
	if !strings.Contains(string(raw), `"kind": "outbound"`) {
		t.Errorf("信封必须标 kind=outbound")
	}
	if !strings.Contains(string(raw), `"sanitize": true`) {
		t.Errorf("信封必须标 sanitize 实际取值（11128 的第一嫌疑）")
	}
}

// TestDumpBlockedBodyNoDir 未配置目录（测试/嵌入）时不落盘、不 panic。
func TestDumpBlockedBodyNoDir(t *testing.T) {
	h := &Handler{cfg: Config{}}
	if p := h.dumpBlockedBody("m", "inbound", "{}", true); p != "" {
		t.Fatalf("未配置目录不应写文件，got %s", p)
	}
}
