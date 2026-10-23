// dump.go 上游原始报文旁路落盘（诊断用，默认关闭）。
//
// 背景（issue #149）：客户端看到「带 tools 的 agentic 请求返回 stop + 零 tool_calls」，
// 但 request-logs 只有元数据（设计如此：不写正文），dsml 修复命中日志又在 stderr，
// 于是没有人能回答「上游到底发过 tool_calls 没有」。本文件给运维一个显式开关：
// Client.DumpDir（WB2A_DUMP_UPSTREAM）非空时，每次 chat 请求落一个自包含文件——
// 出站请求体（我们真正发给上游的东西）+ 上游原始 SSE 字节，不经任何改写。
//
// 只有显式配置目录才落盘：正文含用户提示词，不能默认开。单个文件封顶 8 MB
// （只停写不掐流，响应照常透传给客户端），写盘失败只记一行日志、绝不影响请求。
package upstream

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// dumpLimit 单文件封顶：请求体 + 响应体合计。
const dumpLimit = 8 << 20

// dumpRequestMax 请求体片段上限默认值：足够看清 tools / messages 结构，又不至于
// 吃掉整个配额。Client.DumpRequestMax 可覆盖（长会话把 tools 排在几百条 messages
// 之后，默认值常在中途截断）。
const dumpRequestMax = 1 << 18

// dumpUpstream 在 DumpDir 为空时原样返回 rc（零开销、零行为变化）。
func (c *Client) dumpUpstream(rc io.ReadCloser, prepared []byte, a *auth.Auth) io.ReadCloser {
	if c.DumpDir == "" {
		return rc
	}
	if err := os.MkdirAll(c.DumpDir, 0o700); err != nil {
		log.Printf("WARN: [upstream] dump dir %s: %v", c.DumpDir, err)
		return rc
	}
	f, err := os.OpenFile(filepath.Join(c.DumpDir, dumpName(a)), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		log.Printf("WARN: [upstream] dump open: %v", err)
		return rc
	}
	reqMax := c.DumpRequestMax
	if reqMax <= 0 {
		reqMax = dumpRequestMax
	}
	head := prepared
	if len(head) > reqMax {
		head = head[:reqMax]
	}
	f.Write([]byte("=== request ===\n"))
	f.Write(head)
	f.Write([]byte("\n=== response ===\n"))
	return &dumpReader{rc: rc, f: f, left: dumpLimit - int64(len(head))}
}

// dumpName 用「时间-域-账号前 8 位」拼文件名：可排序、不落完整 UID。
func dumpName(a *auth.Auth) string {
	tag := "unknown"
	if a != nil {
		tag = a.Realm()
		if uid := a.UID; uid != "" {
			if len(uid) > 8 {
				uid = uid[:8]
			}
			tag += "-" + uid
		}
	}
	return time.Now().Format("20060102-150405.000") + "-" + tag + ".log"
}

// dumpReader 边透传边写盘；配额用尽后只透传。Close 关闭文件。
type dumpReader struct {
	rc   io.ReadCloser
	f    *os.File
	left int64
	once sync.Once
}

func (d *dumpReader) Read(p []byte) (int, error) {
	n, err := d.rc.Read(p)
	if n > 0 && d.left > 0 {
		w := p[:n]
		if int64(len(w)) > d.left {
			w = w[:d.left]
		}
		d.f.Write(w)
		d.left -= int64(len(w))
	}
	return n, err
}

func (d *dumpReader) Close() error {
	err := d.rc.Close()
	d.once.Do(func() { d.f.Close() })
	return err
}
