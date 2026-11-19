// instance.go 网关实例身份探针（W6-H-02 的 Go 侧配合）。
//
// 用途：Android 壳在把 admin_api_key 注入 WebView 的 localStorage 之前，必须先确认
// 「127.0.0.1:<端口> 对面确实是我拉起的那个网关进程」。回环在 Android 上跨 App 共享：
// 任一持 INTERNET 权限的 App 先 bind 端口，用户点「更多 → 网页面板」就把管理员密钥
// 交给了它。判据是本进程私有目录里的一个随机串 —— 第三方 App 读不到，也就报不出来。
//
// 契约（Java 侧见 apk-src/java/com/workbuddy/api/InstanceId.java）：
//
//	GET /panel/api/instance → {"instance_id":"<32 位小写 hex>"}
//	Go 从 <工作目录>/data/instance_id 读同一个文件；不存在则生成后原子写（0600）。
//
// 三条硬约束：
//  1. **只回环**（RemoteAddr）：非回环一律 403 —— 这个端点不需要任何密钥，能远程调
//     就等于给公网一个指纹面。
//  2. **免鉴权**：调用方在拿到 admin key（甚至任何 key）之前就要用它，所以它排在
//     Bearer 之前（panelPublicPath 的唯一 API 例外）。
//  3. **绝不回显请求里的任何东西**：不接受 query/body/header 参数。哪怕只是把
//     ?instance_id= 回显一下，占位进程就能原样反射通过校验。
package panel

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
)

// instanceIDFile 实例标识文件名（相对 <工作目录>/data/）。
const instanceIDFile = "instance_id"

// instanceIDLen 32 位小写 hex = 16 字节随机。与 Java 侧 HEX32 的判据一致：
// 长度或字符集不对就当「没有」，重新生成 —— 半截串会让校验永远不匹配且没有任何提示。
const instanceIDLen = 32

// instanceIDPath 返回实例标识文件路径。
//
// 优先用 Config.InstanceIDPath（测试与嵌入式用法显式指定）；否则按网关约定取
// <config.json 同目录>/data/instance_id —— Android 上就是 files/gateway/data/instance_id，
// 与壳的 InstanceId.file() 完全同一个文件。
func (p *Panel) instanceIDPath() string {
	if v := strings.TrimSpace(p.cfg.InstanceIDPath); v != "" {
		return v
	}
	if strings.TrimSpace(p.cfg.ConfigPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.cfg.ConfigPath), "data", instanceIDFile)
}

// validInstanceID 32 位小写 hex。
func validInstanceID(s string) bool {
	if len(s) != instanceIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// loadOrCreateInstanceID 读已有 id；缺失/损坏/超长则生成并原子落盘。
//
// 用 safefile.Write（临时文件 + fsync + rename）：进程被系统杀掉时留下的是旧文件或
// 新文件，不会出现半截串。加锁是必须的 —— 两个并发请求各自生成一个 id，其中一个
// 会写赢文件，于是「HTTP 报的 id」与「壳读到的文件内容」不一致，注入路径永远失败。
func (p *Panel) loadOrCreateInstanceID() (string, error) {
	path := p.instanceIDPath()
	if path == "" {
		return "", errors.New("instance_id 路径未配置（ConfigPath 与 InstanceIDPath 都为空）")
	}
	p.instanceMu.Lock()
	defer p.instanceMu.Unlock()
	// 进程内缓存：正常路径下这个文件一辈子不变，没必要每个请求都 stat 一次。
	if p.instanceID != "" {
		return p.instanceID, nil
	}
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); validInstanceID(id) {
			p.instanceID = id
			return id, nil
		}
		// 内容不合法（半截串/被人写坏/塞了别的东西）：当作没有，重新生成。
	} else if !errors.Is(err, os.ErrNotExist) {
		// 权限错误等：不覆盖，直接把问题报上去（覆盖会把「目录权限坏了」变成
		// 「id 变了」，壳那边只会看到校验失败，查不到根因）。
		return "", fmt.Errorf("读取 instance_id: %w", err)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 instance_id: %w", err)
	}
	id := hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("创建 data 目录: %w", err)
	}
	if err := safefile.Write(path, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("写入 instance_id: %w", err)
	}
	p.instanceID = id
	return id, nil
}

// instanceInfo GET /panel/api/instance —— 只回本进程的实例标识。
//
// 不读 query、不读 body、不回显任何请求内容（见文件头第 3 条约束）。
func (p *Panel) instanceInfo(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeErr(w, http.StatusForbidden,
			"instance_local_only：实例标识只对本机回环开放（用于注入前确认对面身份）")
		return
	}
	id, err := p.loadOrCreateInstanceID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "instance_id_unavailable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"instance_id": id})
}
