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
	// 写失败必须知道（审查 A1-22）：此前三处 f.Write 的返回值全部丢弃，磁盘满/
	// 配额耗尽时会得到一个**看起来完整、实际被截断**的 dump 文件 —— 排障时按它
	// 判断「上游发过什么」直接得出错误结论。这里一有写错误就放弃落盘并回退成
	// 纯透传（绝不因为诊断功能影响请求本身）。
	if _, werr := f.Write([]byte("=== request ===\n")); werr != nil {
		log.Printf("WARN: [upstream] dump write header: %v（放弃落盘，继续透传）", werr)
		_ = f.Close()
		return rc
	}
	if _, werr := f.Write(head); werr != nil {
		log.Printf("WARN: [upstream] dump write request body: %v（放弃落盘，继续透传）", werr)
		_ = f.Close()
		return rc
	}
	if _, werr := f.Write([]byte("\n=== response ===\n")); werr != nil {
		log.Printf("WARN: [upstream] dump write separator: %v（放弃落盘，继续透传）", werr)
		_ = f.Close()
		return rc
	}
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
	// warnOnce 写盘失败只告警一次（审查 A1-22）：ENOSPC 之后每次 Read 都会失败，
	// 逐次打日志会把 stderr 刷爆，反而盖住真正的问题。失败后不再尝试写盘。
	warnOnce sync.Once
}

func (d *dumpReader) Read(p []byte) (int, error) {
	n, err := d.rc.Read(p)
	if n > 0 && d.left > 0 {
		w := p[:n]
		if int64(len(w)) > d.left {
			w = w[:d.left]
		}
		if _, werr := d.f.Write(w); werr != nil {
			// 磁盘满/配额耗尽：停写并只告警一次（透传不受影响）。
			d.left = 0
			d.warnOnce.Do(func() {
				log.Printf("WARN: [upstream] dump write: %v（dump 文件不完整，后续只透传）", werr)
			})
		} else {
			d.left -= int64(len(w))
		}
	}
	return n, err
}

func (d *dumpReader) Close() error {
	err := d.rc.Close()
	d.once.Do(func() { d.f.Close() })
	return err
}
