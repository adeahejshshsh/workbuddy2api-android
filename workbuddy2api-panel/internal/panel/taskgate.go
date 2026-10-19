package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ── 任务跳过名单（v1.22.0）────────────────────────────────────────────
//
// 目的：任务中心**只做能做得了的**。
//
//	① 尝试型动作（autoAction.Attempt：上游未证实可脚本化）默认不进待办 ——
//	   以前它们照旧进队列、照旧执行、照旧不点亮，用户看到的就是"做了没用"；
//	② 某个账号上**反复执行仍无进展**的 (账号,任务码) 自动跳过（3 次 / 冷却 7 天）——
//	   上游不计数的行为不该被无限重试。
//
// 为什么不静默过滤：v1.20.0 的教训 —— 被吞掉的原因会让用户以为"按钮没生效"。
// 所以跳过的项在扫描结果里**列出来并写明原因**，队列诊断账里也计入 skipped，
// 面板上还有「清空跳过名单」一键重试。状态落盘 data/task_skip.json。
const (
	skipFailLimit = 3                  // 连续无效几次后跳过
	skipCooldown  = 7 * 24 * time.Hour // 冷却：到期自动放回待办（"做不了"不该是永久判决）
)

// skipEntry 一条跳过记录（key = uid|code）。
type skipEntry struct {
	Fails int       `json:"fails"` // 连续无效次数（有进展会清零）
	Why   string    `json:"why"`   // 最后一次无效的原因（给用户看）
	Last  time.Time `json:"last"`
	Until time.Time `json:"until"` // 跳过截止（零值 = 还没到跳过线）
}

type skipState struct {
	AllowAttempt bool                  `json:"allow_attempt"` // 是否执行尝试型任务（默认 false）
	Entries      map[string]*skipEntry `json:"entries"`
}

func skipKey(uid, code string) string { return uid + "|" + code }

type skipStore struct {
	mu   sync.Mutex
	path string     // 空 = 纯内存（测试/未配置）
	st   *skipState // 加载一次后常驻
}

func (p *Panel) sk() *skipStore {
	p.skipOnce.Do(func() { p.skip = &skipStore{path: p.cfg.SkipFile} })
	return p.skip
}

// load 读盘（锁内调用）。文件不存在/损坏 → 空表：绝不因为一个状态文件把面板搞崩。
func (s *skipStore) load() *skipState {
	if s.st != nil {
		return s.st
	}
	st := &skipState{Entries: map[string]*skipEntry{}}
	if s.path != "" {
		if raw, err := os.ReadFile(s.path); err == nil && len(raw) > 0 {
			var got skipState
			if json.Unmarshal(raw, &got) == nil {
				if got.Entries == nil {
					got.Entries = map[string]*skipEntry{}
				}
				st = &got
			}
		}
	}
	s.st = st
	return st
}

// save 落盘（原子：临时文件 + rename；失败不影响内存语义）。
func (s *skipStore) save(st *skipState) {
	s.st = st
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// attemptAllowed 尝试型任务是否放行（面板开关，默认关 = 只留有把握的）。
func (p *Panel) attemptAllowed() bool {
	s := p.sk()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load().AllowAttempt
}

func (p *Panel) setAllowAttempt(v bool) {
	s := p.sk()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	st.AllowAttempt = v
	s.save(st)
}

// skipBlocked 该 (账号,任务码) 是否在跳过期内。到期自动放回（并落盘）。
func (p *Panel) skipBlocked(uid, code string) (bool, string) {
	if uid == "" || code == "" {
		return false, ""
	}
	s := p.sk()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	k := skipKey(uid, code)
	e := st.Entries[k]
	if e == nil || e.Fails < skipFailLimit {
		return false, ""
	}
	if e.Until.IsZero() || time.Now().Before(e.Until) {
		return true, e.Why
	}
	delete(st.Entries, k) // 冷却到期 → 再给它一轮机会
	s.save(st)
	return false, ""
}

// skipNote 记一次"执行了但没进展"。返回 (是否刚被跳过, 累计次数)。
func (p *Panel) skipNote(uid, code, why string) (bool, int) {
	if uid == "" || code == "" {
		return false, 0
	}
	s := p.sk()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	k := skipKey(uid, code)
	e := st.Entries[k]
	if e == nil {
		e = &skipEntry{}
		st.Entries[k] = e
	}
	e.Fails++
	e.Why = why
	e.Last = time.Now()
	added := false
	if e.Fails >= skipFailLimit {
		if e.Until.IsZero() {
			added = true
		}
		e.Until = time.Now().Add(skipCooldown)
	}
	s.save(st)
	return added, e.Fails
}

// skipReset 有进展/成功后清零该条（避免"曾经失败过"永远背着）。
func (p *Panel) skipReset(uid, code string) {
	if uid == "" || code == "" {
		return
	}
	s := p.sk()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	k := skipKey(uid, code)
	if _, ok := st.Entries[k]; !ok {
		return
	}
	delete(st.Entries, k)
	s.save(st)
}

// skipClear 清空：uid/code 都为空 = 全清；给了 uid/code = 只清匹配的那些。
func (p *Panel) skipClear(uid, code string) int {
	s := p.sk()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	n := 0
	for k := range st.Entries {
		u, c := splitSkipKey(k)
		if uid != "" && u != uid {
			continue
		}
		if code != "" && c != code {
			continue
		}
		delete(st.Entries, k)
		n++
	}
	s.save(st)
	return n
}

func splitSkipKey(k string) (uid, code string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// skipRows 面板展示用（含是否正在跳过）。
func (p *Panel) skipRows() []map[string]any {
	s := p.sk()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.load()
	now := time.Now()
	out := make([]map[string]any, 0, len(st.Entries))
	for k, e := range st.Entries {
		if e == nil {
			continue
		}
		uid, code := splitSkipKey(k)
		out = append(out, map[string]any{
			"uid":     uid,
			"code":    code,
			"fails":   e.Fails,
			"why":     e.Why,
			"last":    e.Last,
			"until":   e.Until,
			"blocked": e.Fails >= skipFailLimit && (e.Until.IsZero() || now.Before(e.Until)),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i]["code"].(string), out[j]["code"].(string)
		if a != b {
			return a < b
		}
		return out[i]["uid"].(string) < out[j]["uid"].(string)
	})
	return out
}

// tasksSkipState GET/POST /panel/api/tasks/skip
//
//	GET  → {allow_attempt, entries:[…]}
//	POST → {allow_attempt?:bool, clear?:bool, uid?, code?} 统一改开关/清名单
func (p *Panel) tasksSkipState(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "allow_attempt": p.attemptAllowed(),
			"limit": skipFailLimit, "entries": p.skipRows(),
		})
		return
	}
	var body struct {
		AllowAttempt *bool  `json:"allow_attempt"`
		Clear        bool   `json:"clear"`
		UID          string `json:"uid"`
		Code         string `json:"code"`
	}
	limitJSONBody(w, r) // 面板 JSON 接口请求体上限 1MB（M6）
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if body.AllowAttempt != nil {
		p.setAllowAttempt(*body.AllowAttempt)
	}
	cleared := 0
	if body.Clear {
		cleared = p.skipClear(body.UID, body.Code)
	}
	log.Printf("panel: 跳过名单更新 allow_attempt=%v cleared=%d", p.attemptAllowed(), cleared)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "cleared": cleared, "allow_attempt": p.attemptAllowed(),
		"limit": skipFailLimit, "entries": p.skipRows(),
	})
}
