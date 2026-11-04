// admin_audit.go /admin 操作追加一行 JSONL。缺省关闭。写失败不失败请求。
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type AuditLog struct {
	path  string
	keyFP string
	mu    sync.Mutex
}

func NewAuditLog(path, apiKey string) (*AuditLog, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("审计日志路径为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建审计日志目录 %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志 %s: %w", path, err)
	}
	_ = f.Close()
	return &AuditLog{path: path, keyFP: keyFingerprint(apiKey)}, nil
}

func (a *AuditLog) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

func keyFingerprint(apiKey string) string {
	if apiKey == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:4])
}

type auditEntry struct {
	TS     string `json:"ts"`
	Action string `json:"action"`
	Target string `json:"target"`
	Status int    `json:"status"`
	Remote string `json:"remote"`
	Key    string `json:"key"`
	Reason string `json:"reason,omitempty"`
}

func (a *AuditLog) record(e auditEntry) {
	if a == nil {
		return
	}
	e.Key = a.keyFP
	line, err := json.Marshal(e)
	if err != nil {
		log.Printf("[audit] 序列化失败，本条未落盘（操作已生效）：%v", err)
		return
	}
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rotateLocked()
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("[audit] 打开 %s 失败，本条未落盘（操作已生效）：%v", a.path, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		log.Printf("[audit] 写入 %s 失败，本条未落盘（操作已生效）：%v", a.path, err)
	}
}

// auditMaxBytes 单个审计文件的上限（8MB）。超过就滚动：path → path.1 → path.2（保留两份）。
//
// 为什么要滚动：审计只记 /admin 操作，量极小，但**永不滚动**的日志在长期运行的设备上
// 就是一颗定时炸弹（磁盘写满后网关连 state.json 都落不了盘）。8MB 对这类低频日志
// 意味着几十万条操作，正常用不到，只在异常刷屏时兜底。
const auditMaxBytes = 8 << 20

// rotateLocked 超过上限就把当前文件滚动一份（调用方已持锁）。
// 只保留 .1 / .2 两份：审计的价值在「最近发生了什么」，留太多既占盘又难翻。
func (a *AuditLog) rotateLocked() {
	st, err := os.Stat(a.path)
	if err != nil || st.Size() < auditMaxBytes {
		return
	}
	// 从最老的一份开始往后挪，任何一步失败都只是少留一份历史，不影响继续记。
	_ = os.Rename(a.path+".1", a.path+".2")
	_ = os.Rename(a.path, a.path+".1")
	log.Printf("[audit] %s 已达 %d 字节，滚动为 .1（更早的 .1 顺延为 .2）", a.path, st.Size())
}

const auditBodyPeek = 4 << 10

// audit 挂在 withAuth 之内：匿名请求不落盘。
func (h *Handler) audit(action string, target func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	if h.cfg.Audit == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		reason := peekReason(r)
		rec := &auditResponseWriter{ResponseWriter: w}
		next(rec, r)
		h.cfg.Audit.record(auditEntry{
			TS:     time.Now().Format(time.RFC3339),
			Action: action,
			Target: target(r),
			Status: rec.statusCode(),
			Remote: r.RemoteAddr,
			Reason: reason,
		})
	}
}

func auditPathValue(field string) func(*http.Request) string {
	return func(r *http.Request) string { return r.PathValue(field) }
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *auditResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func peekReason(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf, _ := io.ReadAll(io.LimitReader(r.Body, auditBodyPeek))
	r.Body = auditBody{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
	var body struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(buf, &body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

type auditBody struct {
	io.Reader
	io.Closer
}
