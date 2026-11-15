// sse_line.go SSE 单行读取的**长度上限**唯一入口（审查 W4-U1-02 / F-11）。
//
// **为什么需要这一层**：SSE 的两条读循环（Aggregate 非流式聚合、StreamHint 流式
// 透传）此前都是
//
//	br := bufio.NewReaderSize(r, 64*1024)
//	line, err := br.ReadString('\n')
//
// `bufio.Reader` 的 64KB 只约束**内部读块**，不约束**返回的行**：ReadString 内部是
// 一个 `collectFragments` 循环，缓冲区满就再分配、继续攒，直到遇到 '\n' 为止。于是
// 「一条不含换行的超长行」会让网关按行长线性增长分配内存 —— 上游（或被中间人篡改的
// 链路、或一个只回 `<html>...` 错误页且没有换行的 WAF）发一条 GB 级无换行数据，就能
// 把单请求内存打满，进而 OOM 掉整个进程（Go 的 OOM 是进程级，不是请求级）。
//
// 这里把「读一行」收敛成带硬上限的实现：超过上限即**排空本行剩余字节后**返回
// errSSELineTooLong，内存占用恒定。排空是必需的 —— 若不排空就返回，下一次读会从
// 半行中间开始，后续所有行都会错位解析成垃圾（比直接失败更难查）。
package upstream

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
)

// MaxSSELineBytes 单条 SSE 行的硬上限（8MB）。
//
// 取值依据：正常上游一行就是**一个 chunk**，实测最大的是带 base64 内联图片的多模态
// 帧（数百 KB 量级）；8MB 留了一个数量级余量，同时远小于任何部署的容器内存。
const MaxSSELineBytes = 8 << 20

// errSSELineTooLong 上游发来的单行超过 MaxSSELineBytes（协议异常，不是传输错误）。
var errSSELineTooLong = errors.New("upstream sse line exceeds MaxSSELineBytes")

// IsSSELineTooLong 报告错误是否为「上游单行超长」。
// 调用方据此把归因说清楚（是上游发了畸形超长行，不是我们读超时/断流）。
func IsSSELineTooLong(err error) bool { return errors.Is(err, errSSELineTooLong) }

// readLineLimited 读一行（语义与 bufio.Reader.ReadString('\n') 对齐：返回内容**含**
// 行尾 '\n'；EOF 时返回残留内容 + io.EOF；无残留时返回 "" + io.EOF），但给行长设硬上限。
//
// max<=0 时用 MaxSSELineBytes。超限时返回 ("", errSSELineTooLong) 并已排空本行。
func readLineLimited(br *bufio.Reader, max int) (string, error) {
	if max <= 0 {
		max = MaxSSELineBytes
	}
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(frag) > 0 {
			if len(buf)+len(frag) > max {
				// 超限：本行已经不可能被正确解析。把剩余部分读完丢弃（O(1) 内存），
				// 保证下一次读从行边界开始；否则后续每一行都会错位成垃圾。
				for err == bufio.ErrBufferFull {
					_, err = br.ReadSlice('\n')
				}
				log.Printf("WARN: [upstream] SSE 单行超过 %d 字节上限，按超长帧丢弃（本行剩余字节已排空）", max)
				return "", fmt.Errorf("%w (max=%d)", errSSELineTooLong, max)
			}
			buf = append(buf, frag...)
		}
		if err == bufio.ErrBufferFull {
			// 缓冲区满但还没遇到 '\n'：继续攒（frag 指向 bufio 内部缓冲，
			// 下一次 ReadSlice 会覆盖它，所以必须 append 复制）。
			continue
		}
		if err != nil {
			// EOF 且手里有残留：与 ReadString 一致，把残留交出去并回 io.EOF，
			// 让调用方按「最后一行没有换行」处理（SSE 里这是常见的正常收尾）。
			if err == io.EOF && len(buf) > 0 {
				return string(buf), io.EOF
			}
			return "", err
		}
		return string(buf), nil
	}
}
