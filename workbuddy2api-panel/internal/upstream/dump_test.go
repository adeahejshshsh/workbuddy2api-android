package upstream

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// DumpDir 为空时零改动：同一个 reader 原样返回，不建目录不写文件。
func TestDumpUpstreamDisabled(t *testing.T) {
	c := New()
	rc := io.NopCloser(strings.NewReader("data: x\n\n"))
	if got := c.dumpUpstream(rc, []byte(`{}`), nil); got != io.ReadCloser(rc) {
		t.Fatalf("DumpDir 为空时应原样返回 reader")
	}
}

// 打开后一次请求一个自包含文件：出站请求体 + 上游原始字节，且字节不被改写。
func TestDumpUpstreamWritesRaw(t *testing.T) {
	dir := t.TempDir()
	c := New()
	c.DumpDir = dir
	raw := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\ndata: [DONE]\n\n"
	a := &auth.Auth{UID: "0011223344556677", Domain: "www.codebuddy.cn"}

	rc := c.dumpUpstream(io.NopCloser(strings.NewReader(raw)), []byte(`{"model":"m"}`), a)
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != raw {
		t.Fatalf("透传字节被改写: %q err=%v", got, err)
	}
	rc.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if len(files) != 1 {
		t.Fatalf("期望 1 个 dump 文件，得到 %d", len(files))
	}
	b, _ := os.ReadFile(files[0])
	s := string(b)
	if !strings.Contains(s, `{"model":"m"}`) || !strings.HasSuffix(s, raw) {
		t.Fatalf("dump 文件应含请求体与原始响应，实际 %q", s)
	}
	if strings.Contains(filepath.Base(files[0]), "0011223344556677") {
		t.Fatalf("文件名不应含完整 UID: %s", files[0])
	}
}

// DumpRequestMax 覆盖默认 256 KB 请求体上限：片段按配置截断，响应段不受影响。
func TestDumpUpstreamRequestMaxOverride(t *testing.T) {
	dir := t.TempDir()
	c := New()
	c.DumpDir = dir
	c.DumpRequestMax = 16
	raw := "data: [DONE]\n\n"

	req := `{"model":"m","tools":[{"type":"function","function":{"name":"read_file"}}]}`
	rc := c.dumpUpstream(io.NopCloser(strings.NewReader(raw)), []byte(req), nil)
	io.Copy(io.Discard, rc)
	rc.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if len(files) != 1 {
		t.Fatalf("期望 1 个 dump 文件，得到 %d", len(files))
	}
	b, _ := os.ReadFile(files[0])
	s := string(b)
	if !strings.Contains(s, req[:16]) || strings.Contains(s, `"read_file"`) {
		t.Fatalf("请求段应截断到 16 字节，实际 %q", s)
	}
	if !strings.HasSuffix(s, raw) {
		t.Fatalf("响应段不应受请求上限影响，实际 %q", s)
	}
}
