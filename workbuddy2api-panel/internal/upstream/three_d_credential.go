// three_d_credential.go 混元 3D 的临时云凭证从哪来。
//
// 混元 3D 走官方 `agenttool/v1/tcproxy`，TC3 签名的 Credential 形如
// `hy-3d.<clientTempToken>`，其中 clientTempToken 是 `ck_t_...` 形式的**短期云服务凭证**，
// 由官方客户端的 `connect_cloud_service`（bundled MCP server `connector-proxy` 的远程工具）
// 换取。签发协议已从官方桌面客户端（5.9.3，app.asar 内 connector-proxy 实现）静态提取，
// 并于 2026-11-14 用真实账号完整验证（签发 → 提交 → DONE → 计费）：
//
//	POST {chatBase}/v2/api-keys
//	Authorization: Bearer <账号 accessToken>（企业号另带 X-Enterprise-Id）
//	{"name":"workbuddy-connect-cloud-service","expire_in_seconds":900,"temporary":true}
//	→ {"code":0,"data":{"key":"ck_t_...","expires_at":...}}
//
// 这就是官方客户端登录态下的同款行为：网关拿已登录账号自己的 access token 去换，
// 不伪造、不猜测。
//
// 两条硬约束：
//
//  1. 拿不到凭证就不发 3D 请求。签发失败一路透传成 503（ErrThreeDNoCredential），
//     而不是拿账号的 OAuth access token 冒充临时凭证——那会污染账号健康度。
//  2. 凭证不进日志、不进错误串、不进测试夹具。签发诊断只记 key 长度 / 过期时刻，
//     上游拒绝时按普通响应错误透出，不附带 token。
package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ErrThreeDNoCredential 没有可用的 `ck_t_` 临时凭证：3D 功能不可用（不是账号的问题）。
var ErrThreeDNoCredential = errors.New("3D 需要 ck_t_ 临时云凭证：自动签发不可用且未配置手工凭证")

// ThreeDCredential 为某个账号取一份临时云凭证。
//
// 实现方自己负责缓存与刷新；本接口只要求「拿不到时返回错误」，不要返回空串配 nil error。
type ThreeDCredential interface {
	TempToken(ctx context.Context, a *auth.Auth) (string, error)
}

// StaticThreeDCredentials 手工配置的临时凭证（config 里 features.three_d_temp_tokens）。
//
// 键是账号 UID，特殊键 `*` 作为兜底（适用于「不区分账号」的凭证）。官方 TTL 未证实，
// 这里不假装知道有效期：过期后由上游拒绝自然暴露，重新填一份即可。
type StaticThreeDCredentials struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewStaticThreeDCredentials 复制一份配置（之后改配置文件不会悄悄改到运行中的凭证）。
func NewStaticThreeDCredentials(m map[string]string) *StaticThreeDCredentials {
	c := &StaticThreeDCredentials{m: map[string]string{}}
	c.Replace(m)
	return c
}

// Replace 整体替换凭证表（面板保存后热生效）。
func (s *StaticThreeDCredentials) Replace(m map[string]string) {
	if s == nil {
		return
	}
	next := make(map[string]string, len(m))
	for k, v := range m {
		if v = strings.TrimSpace(v); v != "" {
			next[strings.TrimSpace(k)] = v
		}
	}
	s.mu.Lock()
	s.m = next
	s.mu.Unlock()
}

// Empty 是否一份凭证都没有（服务端据此在启动日志里提示功能不可用，但不打印内容）。
func (s *StaticThreeDCredentials) Empty() bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m) == 0
}

func (s *StaticThreeDCredentials) TempToken(_ context.Context, a *auth.Auth) (string, error) {
	if s == nil || a == nil {
		return "", ErrThreeDNoCredential
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v := s.m[a.UID]; v != "" {
		return v, nil
	}
	if v := s.m["*"]; v != "" {
		return v, nil
	}
	return "", ErrThreeDNoCredential
}

// autoKeyMargin 缓存有效期提前量：到期前 60s 就视为不可用，避免把即将过期的
// 凭证签进一次 5 分钟的 3D 任务。
const autoKeyMargin = 60 * time.Second

// autoKeyDefaultTTL 上游响应缺 expires_at（或格式未知）时的兜底有效期。
// 官方请求的是 900s；真机观测上游并不严格按 900s 过期（27 分钟后仍有效），
// 兜底取保守值即可，过期太早只是多发一次签发请求。
const autoKeyDefaultTTL = 14 * time.Minute

type autoKeyEntry struct {
	key       string
	expiresAt time.Time
}

type autoKeyFlight struct {
	done chan struct{}
	key  string
	exp  time.Time
	err  error
}

// AutoThreeDCredentials 自动签发的临时凭证：拿账号自己的 access token 调官方
// /v2/api-keys 换 ck_t_（协议见 FetchClientTempKey），按 UID 缓存到过期前，
// 并发同账号签发做单飞。手工配置的凭证表（manual）优先——填了就直接用，不走签发。
type AutoThreeDCredentials struct {
	mu      sync.Mutex
	cache   map[string]autoKeyEntry
	flights map[string]*autoKeyFlight

	manual *StaticThreeDCredentials
	fetch  func(ctx context.Context, a *auth.Auth) (string, time.Time, error)
}

// NewAutoThreeDCredentials 用给定签发函数构造；manual 可为 nil（纯自动）。
func NewAutoThreeDCredentials(fetch func(ctx context.Context, a *auth.Auth) (string, time.Time, error), manual *StaticThreeDCredentials) *AutoThreeDCredentials {
	return &AutoThreeDCredentials{
		cache:   map[string]autoKeyEntry{},
		flights: map[string]*autoKeyFlight{},
		manual:  manual,
		fetch:   fetch,
	}
}

// ReplaceManual 整体替换手工凭证表（面板保存后热生效；自动签发不受影响）。
func (s *AutoThreeDCredentials) ReplaceManual(m map[string]string) {
	if s == nil {
		return
	}
	s.manual.Replace(m)
}

// Invalidate 丢弃某账号的缓存凭证：上游 401/403 拒绝时调用，下一次重新签发。
func (s *AutoThreeDCredentials) Invalidate(uid string) {
	if s == nil || uid == "" {
		return
	}
	s.mu.Lock()
	delete(s.cache, uid)
	s.mu.Unlock()
}

func (s *AutoThreeDCredentials) TempToken(ctx context.Context, a *auth.Auth) (string, error) {
	if s == nil || s.fetch == nil || a == nil {
		return "", ErrThreeDNoCredential
	}
	// 手工凭证优先：填了就完全不签发（显式覆盖自动行为的出口）。
	if k, err := s.manual.TempToken(ctx, a); err == nil {
		return k, nil
	}
	uid := a.UID
	s.mu.Lock()
	if e, ok := s.cache[uid]; ok && time.Now().Before(e.expiresAt.Add(-autoKeyMargin)) {
		s.mu.Unlock()
		return e.key, nil
	}
	if s.flights == nil {
		s.flights = map[string]*autoKeyFlight{}
	}
	if f, ok := s.flights[uid]; ok {
		s.mu.Unlock()
		select {
		case <-f.done:
			if f.err != nil {
				return "", f.err
			}
			return f.key, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f := &autoKeyFlight{done: make(chan struct{})}
	s.flights[uid] = f
	s.mu.Unlock()

	func() {
		defer func() {
			if r := recover(); r != nil {
				f.key, f.err = "", fmt.Errorf("3d temp key panic: %v", r)
			}
			s.mu.Lock()
			delete(s.flights, uid)
			if f.err == nil {
				s.cache[uid] = autoKeyEntry{key: f.key, expiresAt: f.exp}
			}
			s.mu.Unlock()
			close(f.done)
		}()
		f.key, f.exp, f.err = s.fetch(ctx, a)
		if f.err == nil && strings.TrimSpace(f.key) == "" {
			f.err = ErrThreeDNoCredential
		}
		if f.err == nil && f.exp.IsZero() {
			f.exp = time.Now().Add(autoKeyDefaultTTL)
		}
	}()
	if f.err != nil {
		return "", f.err
	}
	return f.key, nil
}
