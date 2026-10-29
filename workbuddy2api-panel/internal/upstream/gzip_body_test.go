package upstream

// 出站 chat body gzip（上游 2026-10-28 `ee1c80d` #8）：5.9.3 真机 chat body 是 gzip
// 压缩的（头 `Content-Encoding: gzip` + body 以 1f 8b 开头），旧实现发明文 JSON ——
// body 字节与编码头两处都与客户端不同，是 11140 指纹面之一。

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestGzipBodyRoundTrip(t *testing.T) {
	src := []byte(strings.Repeat(`{"messages":[{"role":"user","content":"hi"}]}`, 40))
	out, err := gzipBody(src)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if len(out) < 2 || out[0] != 0x1f || out[1] != 0x8b {
		t.Fatalf("不是 gzip 流（应以 1f 8b 开头）: % x", out[:2])
	}
	zr, err := gzip.NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	defer zr.Close()
	back, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压: %v", err)
	}
	if !bytes.Equal(back, src) {
		t.Fatalf("解压后与原文不一致: %d vs %d 字节", len(back), len(src))
	}
}

// 阈值守卫：小 body 不该压（压缩收益为负，客户端亦非一律压）。
func TestGzipThresholdGuard(t *testing.T) {
	if gzipMinChatBody != 1024 {
		t.Fatalf("阈值应为 1024，got %d", gzipMinChatBody)
	}
}
