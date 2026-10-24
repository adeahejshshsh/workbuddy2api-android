// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - theme.js    首屏主题预热（<head> 同步加载，早于样式表）
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
package panel

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
)

//go:embed index.html
var indexHTML []byte

//go:embed theme.js
var themeJS []byte

//go:embed app.js
var appJS []byte

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// contentETag 静态资源内容指纹（sha256 前 8 字节）。
// 包级变量只算一次：资源是 go:embed 的常量，内容不会在进程生命周期内变化。
func contentETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// 内容指纹（启动即算好，避免每次请求重算 300KB 的 sha256）。
var (
	indexHTMLETag = contentETag(indexHTML)
	appJSETag     = contentETag(appJS)
)

// writeStaticAsset 输出静态资源并处理条件请求（bug C2）。
//
// 为什么必须带缓存校验：app.js / index.html 与二进制同体，URL 却固定不变
// （引用路径没有版本串）。换二进制升级后，浏览器的启发式缓存可能继续用旧 JS：
// 新后端 + 旧前端 → 新字段不显示、按钮报 400，用户会以为后端坏了。
// 这里用 no-cache + ETag：每次都回源校验，内容没变就回 304，既不跑旧代码也不浪费带宽。
func writeStaticAsset(w http.ResponseWriter, r *http.Request, contentType, etag string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, tag := range strings.Split(inm, ",") {
			// 兼容弱校验前缀 W/
			if strings.TrimPrefix(strings.TrimSpace(tag), "W/") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	body, etag := p.versionedIndex()
	writeStaticAsset(w, r, "text/html; charset=utf-8", etag, body)
}

// 版本化页面的缓存挂在 Panel 实例上（review #24）：cfg.Version 在单实例内不变，
// 但包级单例会让同进程的第二个 Panel 复用第一个的版本串/ETag。

// versionedIndex 返回带资源版本串的 index.html。
//
// 为什么光靠 ETag + no-cache 不够（misakano/qcgzxw `1ab95fad`）：面板常挂在
// Cloudflare 这类 CDN 后，`.js` 被边缘缓存数小时；升级二进制不会让 CDN 回源
// —— 用户拿到「新页面 + 旧 app.js」，新按钮没有事件绑定，表现就是「点了没反应」。
// URL 带上版本串后，发版即换 URL，CDN 与浏览器缓存自动失效，不依赖 CDN 是否
// 老实遵守 no-cache。
//
// 版本串用 cfg.Version（1.35.1-202610221657）：每次构建都变，同一版本的重建
// 也不会撞。`go run` 下是 "dev"，此时退化为不带版本（本地开发不缓存）。
func (p *Panel) versionedIndex() ([]byte, string) {
	v := strings.TrimSpace(p.cfg.Version)
	if v == "" || v == "dev" {
		return indexHTML, indexHTMLETag
	}
	p.idxOnce.Do(func() {
		// 只替换内嵌的那处引用：indexHTML 是包级常量，改副本，不动原值。
		body := bytes.ReplaceAll(indexHTML,
			[]byte(`src="app.js"`),
			[]byte(`src="app.js?v=`+url.QueryEscape(v)+`"`))
		p.idxBody = body
		// ETag 必须按**实际输出的字节**算：沿用 indexHTML 的 ETag 会让
		// If-None-Match 与响应体错配（浏览器拿 304 却对应另一份内容）。
		p.idxETag = contentETag(body)
	})
	return p.idxBody, p.idxETag
}

// themeScript 输出首屏主题预热脚本。
//
// 它必须被 <head> 以同步外链方式加载（CSP 禁内联）：晚于样式表就回到「刷新先闪深色」，
// 因为用户的明暗偏好只存在浏览器 localStorage 里，服务端无从得知。
func (p *Panel) themeScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(themeJS)
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	writeStaticAsset(w, r, "text/javascript; charset=utf-8", appJSETag, appJS)
}
