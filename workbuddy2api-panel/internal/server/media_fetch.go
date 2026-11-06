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
	"net/http"
	"net/url"
	"sync"
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

var errMediaBusy = errors.New("成品回读并发已满，请稍后重试")

// slotBody 关闭时归还回读名额（名额要占到字节读完，而不是只占到响应头）。
type slotBody struct {
	io.ReadCloser
	once sync.Once
}

func (b *slotBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { <-mediaFetchSlots })
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
	wait := time.NewTimer(mediaBusyWait)
	defer wait.Stop()
	select {
	case mediaFetchSlots <- struct{}{}:
	case <-wait.C:
		return nil, errMediaBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp, err := mediaFetchHTTP.Do(req)
	if err != nil {
		<-mediaFetchSlots
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error 会把完整（带签名的）URL 拼进错误文本
		}
		return nil, fmt.Errorf("连接 %s 失败：%w", u.Hostname(), err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		<-mediaFetchSlots
		return nil, &mediaStatusError{code: resp.StatusCode}
	}
	resp.Body = &slotBody{ReadCloser: resp.Body}
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取成品失败：%w", err)
	}
	if int64(len(raw)) > limit {
		return nil, &mediaTooLargeError{limit: limit}
	}
	return raw, nil
}
