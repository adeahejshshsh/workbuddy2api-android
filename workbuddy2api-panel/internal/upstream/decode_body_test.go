package upstream

// 显式声明 Accept-Encoding 会关闭 net/http 的透明解压，解码责任回到我们身上。
// 这条用例钉住四种形态 + 未压缩，漏一种就意味着整类接口静默失效。

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

func encodeWith(t *testing.T, enc string, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch enc {
	case "gzip":
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(plain)
		_ = w.Close()
	case "deflate-zlib":
		w := zlib.NewWriter(&buf)
		_, _ = w.Write(plain)
		_ = w.Close()
	case "br":
		w := brotli.NewWriter(&buf)
		_, _ = w.Write(plain)
		_ = w.Close()
	case "deflate-raw":
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		_, _ = w.Write(plain)
		_ = w.Close()
	default:
		return plain
	}
	return buf.Bytes()
}

func TestDecodeBodyIfNeeded(t *testing.T) {
	plain := []byte(`{"code":0,"data":{"ok":true}}`)
	cases := []struct{ enc, algo string }{
		{"", ""},
		{"identity", ""},
		{"gzip", "gzip"},
		{"deflate", "deflate-zlib"},
		{"deflate", "deflate-raw"},
		{"br", "br"},
	}
	for _, c := range cases {
		payload := encodeWith(t, c.algo, plain)
		h := http.Header{}
		if c.enc != "" {
			h.Set("Content-Encoding", c.enc)
		}
		resp := &http.Response{Header: h, Body: io.NopCloser(bytes.NewReader(payload))}
		out := decodeBodyIfNeeded(resp)
		got, err := io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("enc=%q algo=%q 读取失败: %v", c.enc, c.algo, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("enc=%q algo=%q 解码结果不对: %q", c.enc, c.algo, got)
		}
		// 真解码过的才该剥掉编码/长度头；identity / 空等于没编码，留着无害
		decoded := c.enc != "" && c.enc != "identity"
		if decoded && (out.Header.Get("Content-Encoding") != "" || out.Header.Get("Content-Length") != "") {
			t.Fatalf("enc=%q 解码后仍带着 Content-Encoding/Content-Length", c.enc)
		}
	}
}

// 不认识的编码（compress 是 LZW，Go 无内置解码器）必须原样放过 ——
// 「解错」比「不解」更危险：会把它当明文去解析并误判。
func TestDecodeBodyUnknownEncodingPassesThrough(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"compress"}},
		Body:   io.NopCloser(strings.NewReader("not-really-br")),
	}
	out := decodeBodyIfNeeded(resp)
	if out.Header.Get("Content-Encoding") != "compress" {
		t.Fatal("未知编码不该被吞掉")
	}
	got, _ := io.ReadAll(out.Body)
	if string(got) != "not-really-br" {
		t.Fatal("未知编码应原样透传")
	}
}

// 声明值只含 gzip/deflate：真机是四段（含 br），本仓不解 br 就不能声明它。
func TestCommonHeadersAcceptEncoding(t *testing.T) {
	c := New()
	req, _ := http.NewRequest("GET", "https://x.test/v2/x", nil)
	c.CommonHeaders(req, nil)
	ae := req.Header.Get("Accept-Encoding")
	if ae != "gzip, compress, deflate, br" {
		t.Fatalf("Accept-Encoding=%q want 真机四段 \"gzip, compress, deflate, br\"", ae)
	}
}
