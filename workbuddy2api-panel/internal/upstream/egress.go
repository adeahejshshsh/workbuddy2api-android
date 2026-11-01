package upstream

import "net/http"

// do 是**全部出站请求的唯一出口**（13 个调用点收敛到这里）。
//
// 为什么需要这一层：涉及「出站字节形态」的规则（头名拼写、头发送顺序）只应该写一次。
// 此前它们散在 client.go / desktop.go / global_models.go / global_register.go /
// webagent.go / search.go 六处，每加一条规则就要改六遍，漏一处就是一个可被比对的
// 指纹差异（上游 2026-11-01 的 `ee5467c`/`dd2ec95` 就是栽在这个形状上 —— 他们把
// 规则挂在自己的单一出口 proxy.do()，我们这边当时没有这一层）。
//
// 本文件的职责边界：**只做出站前的统一改写**，不改变请求内容、不做重试、不改超时。
// 谁需要按路径区分行为，在调用方决定，不在这里加分支。
func (c *Client) do(req *http.Request) (*http.Response, error) {
	return c.HTTP.Do(req)
}
