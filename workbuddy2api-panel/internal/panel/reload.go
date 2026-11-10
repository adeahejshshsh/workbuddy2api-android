// reload.go 账号目录热重载（issue #199）：auths/ 增删改不必重启进程。
//
// 触发源两条，都走同一个 ReloadAccounts：
//   - 手动：POST /panel/api/accounts/reload（脚本/编排服务的确定性入口）；
//   - 自动：ReloadLoop 按 accounts.reload_interval 轮询（默认 10s，"0s" 关闭）。
//
// 为什么不只用 inotify：群晖/CIFS/NFS 挂载上 inotify 普遍不可靠，轮询必须兜底；
// 反过来只用轮询又不能实时，所以两条都留。
//
// 四条防「踢号」的护栏（issue #199 里点名的风险，逐条对应）：
//
//  1. 有任何文件解析失败 → 本轮**只增不删**。半写文件（登录落盘/SaveAtomic 瞬间）
//     会被当成「文件已删」，把在池账号剔除，而剔除结果会持久化，下次启动也不复活。
//  2. 删除要连续 N 次扫描（默认 2）都确认该 uid 不在，第一次只挂账不删。
//  3. 重载前后各取一次目录指纹（名字+大小+mtime），扫描期间集合变动就放弃本轮
//     ——正写到一半的时刻不该做判断。
//  4. 剔除前再 stat 一次该账号的文件；文件在就跳过。这一条同时堵住「面板刚导入、
//     重载还没看到」的竞争：那条路径刚写出的文件一定在盘上。
//
// 另外还有一条与「自激」有关：auth.LoadDir 对空 realm 会 SaveAtomic 回填，自己
// 就会改 mtime。所以轮询判据用**内容指纹**（uid|expiresAt|realm 的有序哈希）而不是
// mtime —— 回填只改文件不改语义，指纹不变就不会反复重载。
package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ReloadResult 一次重载的结果（直接作为接口响应体）。
type ReloadResult struct {
	Added       int      `json:"added"`
	Updated     int      `json:"updated"`
	Removed     int      `json:"removed"`
	ParseErrors int      `json:"parse_errors"`
	Files       int      `json:"files"`
	Skipped     bool     `json:"skipped"`                // 本轮放弃（目录在扫描期间变动 / 无变化）
	Reason      string   `json:"reason,omitempty"`       // 放弃原因
	Errors      []string `json:"errors,omitempty"`       // 解析失败明细
	RemovedUIDs []string `json:"removed_uids,omitempty"` // 便于脚本核对删的是哪些
}

// errReloadNotConfigured 面板未配置 AuthDir（理论上不会发生）。
var errReloadNotConfigured = errors.New("auth dir 未配置，无法重载")

// ReloadAccounts 扫描 AuthDir 并让池与目录对齐。
//
// 与启动路径的差别只有护栏，加载逻辑完全复用 auth.LoadDirReport（同一套「扫描 +
// 解析 + 坏文件回退 .bak + 空 realm 回填」），避免第二份实现漂移。
func (p *Panel) ReloadAccounts() (ReloadResult, error) {
	return p.ReloadAccountsReason("manual")
}

// ReloadAccountsReason 同 ReloadAccounts，reason 只用于日志（manual / poll）。
func (p *Panel) ReloadAccountsReason(reason string) (ReloadResult, error) {
	if p.cfg.AuthDir == "" {
		return ReloadResult{}, errReloadNotConfigured
	}
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()

	before := dirSignature(p.cfg.AuthDir)
	report, err := auth.LoadDirReport(p.cfg.AuthDir)
	if err != nil {
		return ReloadResult{}, err
	}
	after := dirSignature(p.cfg.AuthDir)
	// 护栏 3 的自纠：LoadDirReport 本身会写盘（空 realm 回填 SaveAtomic、坏文件另存
	// .bad），首轮扫描几乎必然让指纹变一次。这类「自己改的」不算并发写入，重扫一遍
	// 就会稳定——所以给一次重试，而不是当场放弃（否则每次新增文件后首轮都白跑）。
	if before != after {
		before = after
		report, err = auth.LoadDirReport(p.cfg.AuthDir)
		if err != nil {
			return ReloadResult{}, err
		}
		after = dirSignature(p.cfg.AuthDir)
	}
	res := ReloadResult{Files: report.Files, ParseErrors: len(report.ParseError), Errors: report.ParseError}

	// 护栏 3：扫描期间目录集合变了 → 放弃本轮（下一轮再来，代价只有几毫秒）。
	if before != after {
		res.Skipped = true
		res.Reason = "目录在扫描期间发生变化，本轮放弃（下一轮重试）"
		log.Printf("[reload] 放弃（%s）：目录在扫描期间变动", reason)
		return res, nil
	}

	seen := make(map[string]bool, len(report.Auths))
	for _, a := range report.Auths {
		seen[a.UID] = true
		if p.cfg.Pool.AuthByUID(a.UID) == nil {
			res.Added++
		} else {
			res.Updated++
		}
		p.cfg.Pool.Add(a) // upsert：已存在只换凭证，保留 credits/cooling 状态
	}

	// 护栏 1：本轮有解析失败 → 只增不删。此时「不在扫描结果里」可能只是「文件读不动」。
	if res.ParseErrors > 0 {
		res.Skipped = res.Added == 0 && res.Updated == 0
		res.Reason = fmt.Sprintf("%d 个文件解析失败，本轮只增不删", res.ParseErrors)
		p.noteReload(res)
		log.Printf("[reload] %s added=%d updated=%d removed=0 parse_errors=%d（解析失败时禁止删除）",
			reason, res.Added, res.Updated, res.ParseErrors)
		return res, nil
	}

	// 护栏 2：缺席计数满 N 次才删；期间有在途请求也往后推。
	confirm := p.reloadDeleteConfirm
	if confirm < 1 {
		confirm = 1
	}
	var toRemove []string
	for _, st := range p.cfg.Pool.List() {
		if seen[st.UID] {
			delete(p.reloadMissing, st.UID)
			continue
		}
		// 护栏 4：文件其实在盘上（可能是面板刚写入、或扫描竞态）→ 视为在场，不计缺席。
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.FilePath != "" {
			if _, statErr := os.Stat(a.FilePath); statErr == nil {
				delete(p.reloadMissing, st.UID)
				continue
			}
		}
		p.reloadMissing[st.UID]++
		if p.reloadMissing[st.UID] < confirm {
			continue
		}
		if p.cfg.Pool.InFlight(st.UID) > 0 {
			log.Printf("[reload] uid=%s 文件已不存在，但在途请求未结束 → 下一轮再删", st.UID)
			continue
		}
		toRemove = append(toRemove, st.UID)
	}
	if removed := p.cfg.Pool.RemoveMany(toRemove); len(removed) > 0 {
		res.Removed = len(removed)
		res.RemovedUIDs = removed
		for _, uid := range removed {
			delete(p.reloadMissing, uid)
		}
	}
	// 已不在池里的 uid 不再占着缺席计数表。
	for uid := range p.reloadMissing {
		if p.cfg.Pool.AuthByUID(uid) == nil {
			delete(p.reloadMissing, uid)
		}
	}

	p.noteReload(res)
	if res.Added > 0 || res.Updated > 0 || res.Removed > 0 {
		// 一行审计日志：内容变动的重载才打（10s 轮询下没变化时不能刷屏）。
		log.Printf("[reload] added=%d updated=%d removed=%d parse_errors=0 files=%d",
			res.Added, res.Updated, res.Removed, res.Files)
		if len(res.RemovedUIDs) > 0 {
			log.Printf("[reload] 已移除（auth 文件不存在，连续 %d 次扫描确认）: %s",
				confirm, strings.Join(res.RemovedUIDs, ", "))
		}
	}
	return res, nil
}

// noteReload 记录「上次重载时间/结果」供面板展示。
// 调用方必须已持有 reloadMu（ReloadAccountsReason 全程持锁）；这里再加锁就是
// 自己锁自己——sync.Mutex 不可重入，会当场死锁。
func (p *Panel) noteReload(res ReloadResult) {
	p.reloadLastAt = time.Now()
	p.reloadLast = res
}

// ReloadStatus 面板展示用的最近一次重载快照。
func (p *Panel) ReloadStatus() (time.Time, ReloadResult) {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()
	return p.reloadLastAt, p.reloadLast
}

// SetReloadConfig 设置轮询间隔与删除确认次数（cmd/server 按 config 注入）。
func (p *Panel) SetReloadConfig(interval time.Duration, confirm int) {
	p.reloadMu.Lock()
	p.reloadInterval = interval
	if confirm > 0 {
		p.reloadDeleteConfirm = confirm
	}
	p.reloadMu.Unlock()
}

// ReloadLoop 按间隔轮询目录；指纹没变就完全不动池（不扫、不解析、不写日志）。
//
// 指纹只在「可能变了」时才做完整扫描：先取目录指纹（名字+大小+mtime），与上次
// 成功重载后的指纹比对，相同即跳过。这一步不解析文件内容，10s 一次的开销就是
// 一次 ReadDir——几十个账号的目录是小几百微秒级。
func (p *Panel) ReloadLoop(stop <-chan struct{}) {
	p.reloadMu.Lock()
	interval := p.reloadInterval
	p.reloadMu.Unlock()
	if interval <= 0 {
		log.Printf("[reload] 目录轮询未启用（accounts.reload_interval=0）；手动接口 %s 仍可用",
			relooadEndpointPath)
		return
	}
	log.Printf("[reload] 目录轮询已启用：每 %s 检查一次 %s（删除需连续 %d 次确认）",
		interval, p.cfg.AuthDir, p.reloadDeleteConfirm)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// 启动后立刻对齐一次：进程启动到面板起来之间外部可能已经丢了文件进来。
	if res, err := p.reloadIfChanged("poll"); err != nil {
		log.Printf("[reload] 启动首轮重载失败：%v", err)
	} else if res.Added+res.Updated+res.Removed > 0 {
		log.Printf("[reload] 启动首轮：added=%d updated=%d removed=%d", res.Added, res.Updated, res.Removed)
	}
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if _, err := p.reloadIfChanged("poll"); err != nil {
				log.Printf("WARN: [reload] 轮询重载失败：%v", err)
			}
		}
	}
}

// relooadEndpointPath 手动重载接口路径（日志文案用；路由注册在 panel.go）。
const relooadEndpointPath = "POST /panel/api/accounts/reload"

// reloadIfChanged 目录指纹变了才真正重载。
//
// 用内容指纹而不是 mtime：auth.LoadDirReport 对空 realm 会 SaveAtomic 回填，自己
// 就会改 mtime；按 mtime 判断会自激（backfill → mtime 变 → 再 reload → ...）。
func (p *Panel) reloadIfChanged(reason string) (ReloadResult, error) {
	fp := dirContentFingerprint(p.cfg.AuthDir)
	p.reloadMu.Lock()
	last := p.reloadFingerprint
	p.reloadMu.Unlock()
	if fp != "" && fp == last {
		return ReloadResult{Skipped: true, Reason: "目录未变化"}, nil
	}
	res, err := p.ReloadAccountsReason(reason)
	if err != nil {
		return res, err
	}
	if !res.Skipped {
		p.reloadMu.Lock()
		p.reloadFingerprint = fp
		p.reloadMu.Unlock()
	}
	return res, nil
}

// dirSignature 目录「集合指纹」：文件名+大小+mtime 的有序哈希。
//
// 只用来判「扫描期间目录有没有被别人动过」——比内容指纹便宜，且能看见尚未写完的
// 文件（大小/mtime 已变但内容还不完整）。
func dirSignature(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "workbuddy") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		names = append(names, fmt.Sprintf("%s|%d|%d", e.Name(), info.Size(), info.ModTime().UnixNano()))
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:8])
}

// dirContentFingerprint 目录「内容指纹」：uid|expiresAt|realm 的有序哈希。
//
// 比 dirSignature 贵（要解析每个文件），但只在 dirSignature 可能变化时才算——
// 见 reloadIfChanged。它回答的是「账号语义变了吗」，backfill 回填 realm 属于
// 「文件变了但账号没变」的情形，指纹不变，因此不会自激。
func dirContentFingerprint(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "workbuddy") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			lines = append(lines, e.Name()+"|unreadable")
			continue
		}
		a, perr := auth.Parse(raw)
		if perr != nil {
			// 解析失败也进指纹：文件被写坏了要算「变了」，否则永远不会重试。
			lines = append(lines, e.Name()+"|invalid")
			continue
		}
		lines = append(lines, fmt.Sprintf("%s|%s|%d|%s", e.Name(), a.UID, a.ExpiresAt, a.Realm()))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

// reloadAccounts POST /panel/api/accounts/reload —— 手动重载账号目录。
//
// 返回 ReloadResult；目录不可读/未配置时 500。跳过（Skipped=true）也是 200：
// 「本轮没动」是正常结果，脚本不该因为目录正在变动就当失败。
func (p *Panel) reloadAccounts(w http.ResponseWriter, r *http.Request) {
	res, err := p.ReloadAccountsReason("manual")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "重载失败: "+err.Error())
		return
	}
	at, _ := p.ReloadStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"result":   res,
		"at":       at,
		"auth_dir": p.cfg.AuthDir,
		"pool":     len(p.cfg.Pool.List()),
	})
}

// reloadStatus GET /panel/api/accounts/reload —— 上次重载的时间与结果。
//
// 为什么要有 GET：轮询是后台行为，出问题时用户只能看到「账号莫名少了」，看不出
// 「是不是重载删的、删了谁」。这个接口把结果摆出来（含 removed_uids）。
func (p *Panel) reloadStatus(w http.ResponseWriter, r *http.Request) {
	at, res := p.ReloadStatus()
	p.reloadMu.Lock()
	interval := p.reloadInterval
	confirm := p.reloadDeleteConfirm
	missing := len(p.reloadMissing)
	p.reloadMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"at":                  at,
		"result":              res,
		"auth_dir":            p.cfg.AuthDir,
		"interval":            interval.String(),
		"delete_confirm":      confirm,
		"pending_delete":      missing,
		"pool":                len(p.cfg.Pool.List()),
	})
}
