// device_token.go X-Device-Token 的文件兜底读取（与桌面端共用状态文件）。
//
// 容器内无桌面端 Turing Shield SDK，无法像 Python fork（xiaofan6ya/converter.py）那样
// 现取 token。这里提供另一条路径：宿主把桌面端生成的 device token 落 /app/data/device_token
// （或任意挂载路径），网关定期读取注入。读取频率限 5 分钟一次缓存，>1KB 或读失败则忽略
// （优雅降级不注入，不影响主流程）。
package upstream

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"
)

var errDeviceTokenTooLarge = errors.New("device token file too large")

// deviceTokenFile 文件读取缓存 TTL（秒）。桌面端 SDK 自身也有缓存，这里再兜一层
// 避免每次出站请求都 stat+read 文件。
const deviceTokenFileTTL = 5 * time.Minute

// deviceTokenFileMaxLen token 文件最大字节数。token 通常几百字节；超过 1KB
// 视为异常（非 token 内容 / 文件被误用），忽略不注入。
const deviceTokenFileMaxLen = 1024

// deviceTokenFileCache 缓存 device token 文件读取结果（path → token+读取时刻）。
type deviceTokenFileCache struct {
	mu      sync.Mutex
	path    string
	token   string
	readAt  time.Time
	lastErr error
}

var dtFileCache = &deviceTokenFileCache{}

// readDeviceTokenFile 读取并缓存 device token 文件；5 分钟内复用上次结果。
// 返回空串表示无可用 token（文件未配置 / 读失败 / 内容过长 / 空白）。
func readDeviceTokenFile(path string) string {
	if path == "" {
		return ""
	}
	// 快路径：缓存命中且未过期，直接返回缓存值。
	dtFileCache.mu.Lock()
	if path == dtFileCache.path && time.Since(dtFileCache.readAt) < deviceTokenFileTTL {
		tok := dtFileCache.token
		dtFileCache.mu.Unlock()
		return tok
	}
	dtFileCache.mu.Unlock()

	// 审查 A1-06（P2）：**文件 IO 必须在锁外**。
	//
	// 缺陷：原实现 `defer Unlock()` 把整段（含 os.Stat + os.ReadFile）罩在互斥锁里，
	// 而这是每个出站请求都会走的 injectDeviceToken → resolveDeviceToken 路径。
	// 宿主把 token 文件放在慢盘/NFS 上时，一次 stat+read 卡顿会阻塞**全部** chat/billing
	// 出站（所有账号、所有请求），表现为整站请求一起变慢，且从日志上完全看不出
	// 「卡在一个 1KB 的文件读上」。
	//
	// 改成双检：锁内只判缓存，锁外做 IO，回来再锁一次写回。重复 IO 的上限是
	// 「同一瞬间有 N 个 goroutine 同时错过缓存」—— 1KB 本地读，可接受；
	// 真出现 NFS 场景再上 singleflight。
	tok, err := readTrimmedFile(path, deviceTokenFileMaxLen)

	dtFileCache.mu.Lock()
	// 双检：我们做 IO 的这段时间里，别的 goroutine 可能已经把同一 path 刷成了更新鲜
	// 的结果（甚至是不同的 path）。以缓存里那份为准 —— 否则一个慢 IO 的结果会把
	// 更新鲜的值覆盖回去，TTL 也跟着被重置，缓存语义就乱了。
	if path == dtFileCache.path && time.Since(dtFileCache.readAt) < deviceTokenFileTTL {
		cached := dtFileCache.token
		dtFileCache.mu.Unlock()
		return cached
	}
	dtFileCache.path = path
	if err != nil {
		// 读失败：清空缓存 token，避免注入过期/错误的值。
		dtFileCache.token = ""
		dtFileCache.lastErr = err
	} else {
		dtFileCache.token = tok
		dtFileCache.lastErr = nil
	}
	dtFileCache.readAt = time.Now()
	dtFileCache.mu.Unlock()
	if err != nil {
		return ""
	}
	return tok
}

// readTrimmedFile 读文件并 trim 首尾空白，超过 maxLen 返回错误（拒绝过长内容）。
func readTrimmedFile(path string, maxLen int) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > int64(maxLen) {
		return "", errDeviceTokenTooLarge
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
