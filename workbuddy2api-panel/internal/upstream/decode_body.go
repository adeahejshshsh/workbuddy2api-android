package upstream

// 出站响应的统一解码（配合我们在 CommonHeaders 里显式声明的 Accept-Encoding）。
//
// **为什么必须有这一层**：显式设置 Accept-Encoding 会关闭 net/http 的透明解压 ——
// 客户端自己声明了就会自己负责解。漏掉它，所有 JSON 接口直接全废；更糟的是 400 分支
// 会把压缩后的字节当文本做错误分类，可能把「正常的上游报错」判成别的形态去罚号。
// 两处是一个整体，不能只搬半个。

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// decodeBodyIfNeeded 按 Content-Encoding 包一层解码；不认识 / 未压缩时原样返回。
//
// 返回的 body 会**去掉** Content-Encoding / Content-Length 头：调用方看到的是解压后的
// 明文，长度不再对应，留着只会误导（也避免下游再解一次）。
func decodeBodyIfNeeded(resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil {
		return resp
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return resp
	}
	switch enc {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return resp // 不是合法 gzip：原样交给调用方，让它按原文报错
		}
		resp.Body = readCloser{r: zr, c: resp.Body}
	case "deflate":
		// HTTP 的 "deflate" 历史上既可能是 zlib 包装，也可能是裸 flate 流 —— 两种都要认。
		// **必须先整段读进内存**：zlib.NewReader 构造时就会消费头部两字节，失败时那个
		// reader 已经回不去了，再拿它喂 flate 就是半截流（实测报 unexpected EOF）。
		// deflate 只会用在 JSON 接口上，整段读是可控的。
		raw, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if rerr != nil {
			return resp
		}
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			resp.Body = readCloser{r: zr, c: io.NopCloser(nil)}
		} else {
			resp.Body = io.NopCloser(flate.NewReader(bytes.NewReader(raw)))
		}
	case "br":
		// brotli：真机四段里有它，声明了就必须能解（否则上游真回 br 时整类接口解析失败）。
		resp.Body = readCloser{r: brotli.NewReader(resp.Body), c: resp.Body}
	default:
		// compress（LZW）等：HTTP 侧实际不会被选中（Go 无内置解码器），原样放过。
		return resp
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp
}

// readCloser 把解码器与原始 body 绑在一起：Close 关原始 body（解码器本身不再持资源）。
type readCloser struct {
	r io.Reader
	c io.Closer
}

func (rc readCloser) Read(p []byte) (int, error) { return rc.r.Read(p) }
func (rc readCloser) Close() error               { return rc.c.Close() }
