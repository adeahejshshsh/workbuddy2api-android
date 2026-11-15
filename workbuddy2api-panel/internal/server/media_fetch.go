// media_fetch.go 回读上游成品直链的共用出站口：图片转 b64_json / Responses 的 image_generation
// （images.go、imagetool.go）与视频 /content 流式转发（videos.go）都走这里。
//
// 与水印回读（watermark.go）同一套收口，因为同样是「服务端主动发请求」：
//   - 只认上游产物域（watermarkHostSuffixes），直链必然来自上游回执，不需要外网自由；
//   - 不跟随重定向：白名单只校验首跳主机，跟随一次就能被 302 带去任意地址；
//   - 请求绑定调用方的 ctx：客户端断连即取消，不替走掉的人把几十 MB 拉完；
//   - 字节上限只做「超限就报错」，从不静默截断——截断的图片 / 视频比拿不到更糟。
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// 用 var 而不是 const：测试把它压小，避免真造几十 MB 的响应。
var (
	// videoContentMax 视频成品转发的字节上限（实测 4s/768P 约 2.9 MB，给足余量但别让单请求吃满带宽）。
	videoContentMax int64 = 256 << 20
	// mediaFetchMax 图像直链转 base64 时的读取上限（上游出图约 2-4 MB；整张读进内存，所以比视频小得多）。
	mediaFetchMax int64 = 32 << 20
)

// mediaFetchHTTP 取成品直链（COS 签名 URL）的客户端。超时给足：一个 3 MB 的 mp4 走公网慢链路也要几十秒。
var mediaFetchHTTP = &http.Client{
	Timeout:       5 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// mediaFetchSlots 同时回读成品直链的上限。一次视频转发能占一条出站连接、一个 goroutine 几分钟，
// 一次 b64_json 要把整张图读进内存再编码；生产机上还跑着别的服务，不能让持有密钥的人并发刷满出站。
// 等不到名额回 errMediaBusy（/content 映射成 503，b64_json 保留直链）。
var (
	mediaFetchSlots = make(chan struct{}, 4)
	mediaBusyWait   = 10 * time.Second
)

// mediaSlotLeaseTTL 单个回读名额租约的最长占用时间（审查 C9-MF-01）。
//
// 为什么必须有它：名额此前**只**靠调用方 Close 归还 —— 调用方漏 Close、panic 在
// defer 之前、或客户端慢读到连接一直不结束，名额就被永久占住。4 个名额各漏一次，
// 之后所有成品回读（图片 b64_json 与视频 /content）都只能回 503/保留直链，是
// 全局单点故障。租约给「占用」一个上界：超过 TTL 的名额由下一次获取顺手强制收回。
//
// 取值 6 分钟的依据：mediaFetchHTTP 的整体超时是 5 分钟，正常回读一定在 5 分钟内
// 结束；TTL 比它长 1 分钟，保证「还在合法传输中的名额」不会被误收，而真漏掉的
// 名额最多多占 6 分钟。var：测试可直接改租约的过期时刻，不必真等 6 分钟。
var mediaSlotLeaseTTL = 6 * time.Minute

// mediaInFlightBudget 成品回读的全局在途字节预算（审查 C9-MF-02）。
//
// 名额只数「并发几个」，不数「一共占多少内存」：4 个 32MiB 的 b64_json 同时读
// 进内存就是 128MiB 常驻，视频转发更狠。这个预算按**声明长度**（Content-Length）
// 先记账再读，超出即 errMediaBusy —— 与名额同一套错误语义，调用方不必新增分支。
// 长度未知（分块传输）时不预占：宁可少一道保护，也不要因为猜错长度把正常请求拒掉；
// 那种情况仍由 mediaFetchMax / videoContentMax 的读取上限兜底。
var (
	mediaInFlightBudget int64 = 128 << 20
	mediaInFlightBytes  atomic.Int64
)

// reserveMediaBytes 预占在途字节；超预算返回 false（调用方回 errMediaBusy）。
func reserveMediaBytes(n int64) bool {
	if n <= 0 {
		return true
	}
	if mediaInFlightBytes.Add(n) > mediaInFlightBudget {
		mediaInFlightBytes.Add(-n)
		return false
	}
	return true
}

// releaseMediaBytes 归还预占的在途字节。
func releaseMediaBytes(n int64) {
	if n > 0 {
		mediaInFlightBytes.Add(-n)
	}
}

var errMediaBusy = errors.New("成品回读并发已满，请稍后重试")

// mediaSlotLeases 名额租约表：lease id → 过期时刻（审查 C9-MF-01）。
//
// 不变式：通道里每个令牌恰好对应表里一条存活租约。归还与强制回收都走 releaseMediaSlot
// 的「先从表里摘、摘到了才动通道」语义，所以同一个名额不会被归还两次（双还会把
// 容量放大成 5、6…），过期被回收后调用方再 Close 也不会动通道。
type mediaSlotLeases struct {
	mu      sync.Mutex
	seq     uint64
	exp     map[uint64]time.Time
	expired atomic.Uint64 // 观测：因租约过期被强制回收的次数（面板/日志排查用）
}

var mediaLeases = &mediaSlotLeases{exp: map[uint64]time.Time{}}

// mediaSlotFreed 名额归还/被回收的唤醒信号（容量 1，非阻塞发送）。
//
// 为什么不用「阻塞发送到 mediaFetchSlots」等名额：那样「占令牌」和「登记租约」
// 是两个原子操作，中间存在窗口 —— 并发 reclaim 可能把刚拿到、还没登记的令牌
// 当过期名额取走，租约表与通道随即不一致（要么永久漏一个名额，要么把别人的
// 名额还掉）。改成「持锁 tryAcquire（占令牌 + 登记租约一步完成）+ 信号唤醒」，
// 不变式在任何时刻都成立。
var mediaSlotFreed = make(chan struct{}, 1)

// tryAcquire 在锁内一次性完成「占一个令牌 + 登记租约」，返回 lease id。
func (l *mediaSlotLeases) tryAcquire(now time.Time) (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case mediaFetchSlots <- struct{}{}:
		l.seq++
		l.exp[l.seq] = now.Add(mediaSlotLeaseTTL)
		return l.seq, true
	default:
		return 0, false
	}
}

// notifyMediaSlotFreed 唤醒一个正在等名额的调用方（容量 1，绝不阻塞）。
func notifyMediaSlotFreed() {
	select {
	case mediaSlotFreed <- struct{}{}:
	default:
	}
}

// reclaim 强制收回所有已过期的租约，返回收回个数（审查 C9-MF-01）。
//
// 只删表 + 从通道取走一个令牌，不碰持有者的 slotBody：调用方之后 Close 时
// releaseMediaSlot 会发现租约已不在表里，于是不再动通道 —— 双还不可能发生。
func (l *mediaSlotLeases) reclaim(now time.Time) int {
	l.mu.Lock()
	var dead []uint64
	for id, exp := range l.exp {
		if now.After(exp) {
			dead = append(dead, id)
		}
	}
	for _, id := range dead {
		delete(l.exp, id)
	}
	l.mu.Unlock()
	for range dead {
		// 非阻塞取：正常情况下每个存活租约都对应通道里的一个令牌，取不到说明
		// 表与通道已经不一致（不该发生），这时宁可少收回也不要卡住获取路径。
		select {
		case <-mediaFetchSlots:
		default:
		}
	}
	if len(dead) > 0 {
		notifyMediaSlotFreed() // 腾出了名额：叫醒一个等待者
	}
	if n := len(dead); n > 0 {
		l.expired.Add(uint64(n))
		log.Printf("WARN: [media] 强制收回 %d 个过期回读名额租约（调用方未 Close，名额曾被永久占住）", n)
	}
	return len(dead)
}

// release 归还一条租约；租约已被过期回收时什么都不做（幂等）。
func (l *mediaSlotLeases) release(lease uint64) {
	if lease == 0 {
		return
	}
	l.mu.Lock()
	_, live := l.exp[lease]
	delete(l.exp, lease)
	l.mu.Unlock()
	if live {
		<-mediaFetchSlots
		notifyMediaSlotFreed() // 名额空出来了：叫醒一个等待者
	}
}

// acquireMediaSlot 取一个回读名额，返回租约 id（调用方必须交回 releaseMediaSlot）。
//
// 获取时顺手回收过期租约（审查 C9-MF-01）：漏 Close 的名额不等到看门狗轮询，
// 下一个人来拿的时候就先清一遍 —— 这是「几次漏 Close 就永久卡死」的根治点。
func acquireMediaSlot(ctx context.Context) (uint64, error) {
	mediaLeases.reclaim(time.Now())
	if lease, ok := mediaLeases.tryAcquire(time.Now()); ok {
		return lease, nil
	}
	wait := time.NewTimer(mediaBusyWait)
	defer wait.Stop()
	for {
		select {
		case <-mediaSlotFreed:
			// 有名额归还或被回收：再抢一次（可能被别人先抢到，那就继续等）。
		case <-wait.C:
			// 等满仍然没有名额：临放弃前再收一次过期租约。若这一手腾出了名额，
			// 说明「忙」其实是死租约造成的，应当直接成功而不是回 503。
			mediaLeases.reclaim(time.Now())
			if lease, ok := mediaLeases.tryAcquire(time.Now()); ok {
				return lease, nil
			}
			return 0, errMediaBusy
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		if lease, ok := mediaLeases.tryAcquire(time.Now()); ok {
			return lease, nil
		}
	}
}

// releaseMediaSlot 归还回读名额（幂等：同一租约重复调用只归还一次）。
func releaseMediaSlot(lease uint64) { mediaLeases.release(lease) }

// mediaSlotLeaseStats 观测：存活租约数与因过期被强制回收的次数（测试与排查用）。
func mediaSlotLeaseStats() (live int, expired uint64) {
	mediaLeases.mu.Lock()
	live = len(mediaLeases.exp)
	mediaLeases.mu.Unlock()
	return live, mediaLeases.expired.Load()
}

// slotBody 关闭时归还回读名额（名额要占到字节读完，而不是只占到响应头）。
type slotBody struct {
	io.ReadCloser
	once  sync.Once
	lease uint64
}

func (b *slotBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { releaseMediaSlot(b.lease) })
	return err
}

// mediaStatusError 直链回了非 200（含不跟随的 3xx）。
type mediaStatusError struct{ code int }

func (e *mediaStatusError) Error() string { return fmt.Sprintf("直链回 %d", e.code) }

// mediaTooLargeError 成品超过字节上限。
type mediaTooLargeError struct{ limit int64 }

func (e *mediaTooLargeError) Error() string {
	return fmt.Sprintf("成品超过 %d MiB 上限", e.limit>>20)
}

// openMedia 打开成品直链，只返回 200 的响应（调用方负责 Close）。
// 错误信息不带完整 URL：签名直链里的 q-signature 不该进日志或回执。
func openMedia(ctx context.Context, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("直链不是 http(s) URL")
	}
	if !watermarkHostAllowed(u.Hostname()) {
		return nil, fmt.Errorf("直链主机 %s 不在上游产物域内，不回读", u.Hostname())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("构造回读请求失败")
	}
	lease, err := acquireMediaSlot(ctx)
	if err != nil {
		return nil, err
	}
	// 统一兜底归还（审查 C9-MF-03）：此前 Do 失败与非 200 两个分支各自手写
	// `<-mediaFetchSlots`，而名额已改成租约后「谁归还、归还几次」必须只有一处判断。
	// 成功路径把租约转交给 slotBody（lease 置 0），由 Body.Close 归还。
	defer func() {
		if lease != 0 {
			releaseMediaSlot(lease)
		}
	}()
	resp, err := mediaFetchHTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error 会把完整（带签名的）URL 拼进错误文本
		}
		return nil, fmt.Errorf("连接 %s 失败：%w", u.Hostname(), err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &mediaStatusError{code: resp.StatusCode}
	}
	resp.Body = &slotBody{ReadCloser: resp.Body, lease: lease}
	lease = 0 // 归还责任已转交 slotBody
	return resp, nil
}

// fetchMedia 把成品直链整个读进内存（图像转 b64 用；视频走流式转发，见 serveVideoContent）。
// 超过 limit 返回 *mediaTooLargeError，不返回被截断的字节。
func fetchMedia(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	resp, err := openMedia(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, &mediaTooLargeError{limit: limit}
	}
	// 全局在途字节预算（审查 C9-MF-02）：长度已知时先记账再读，defer 立即归还，
	// 保证任何早退分支都不会漏还（漏还会让后续回读永久 503）。
	if reserved := resp.ContentLength; reserved > 0 {
		if !reserveMediaBytes(reserved) {
			return nil, errMediaBusy
		}
		defer releaseMediaBytes(reserved)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取成品失败：%w", err)
	}
	if int64(len(raw)) > limit {
		return nil, &mediaTooLargeError{limit: limit}
	}
	return raw, nil
}
