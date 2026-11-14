// three_d.go WorkBuddy 混元 3D：`ck_t_` 临时云凭证 + TC3 签名，经 agenttool tcproxy。
//
// 凭证来源见 three_d_credential.go：签发协议已从官方桌面客户端 5.9.3 静态提取并真机验证
// （2026-11-14）——网关拿已登录账号的 access token 调官方 /v2/api-keys 换 `ck_t_`，
// 与官方客户端 connect_cloud_service 同款行为；签发失败时一律失败，不退回猜测式凭证。
package upstream

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	threeDProxyPath    = "/agenttool/v1/tcproxy"
	threeDSubmitAction = "SubmitHunyuanTo3DProJob"
	threeDQueryAction  = "QueryHunyuanTo3DProJob"

	// 临时凭证签发（官方客户端 connect_cloud_service 的实际请求，5.9.3 提取 + 真机验证）。
	threeDKeyPath       = "/v2/api-keys"
	threeDKeyName       = "workbuddy-connect-cloud-service"
	threeDKeyTTLSeconds = 900
)

// ThreeDGenerations 提交文生/图生 3D 任务。
func (c *Client) ThreeDGenerations(ctx context.Context, a *auth.Auth, body any) (json.RawMessage, error) {
	return c.threeDCall(ctx, a, threeDSubmitAction, body)
}

// ThreeDTask 查询提交账号下的 3D 任务。
func (c *Client) ThreeDTask(ctx context.Context, a *auth.Auth, jobID string) (json.RawMessage, error) {
	return c.threeDCall(ctx, a, threeDQueryAction, map[string]string{"JobId": jobID})
}

func (c *Client) threeDCall(ctx context.Context, a *auth.Auth, action string, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	// 先取凭证：没有就不发请求（不拿账号 OAuth token 顶替，那样失败会误伤账号健康度）。
	temp, err := c.threeDTempToken(ctx, a)
	if err != nil {
		return nil, err
	}
	endpoint := c.chatBase(a) + threeDProxyPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range threeDHeaders(req.URL.Host, action, raw, temp, time.Now()) {
		req.Header.Set(k, v)
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read 3d response: %w", err)
	}
	var env struct {
		Response json.RawMessage `json:"Response"`
		Error    any             `json:"error"`
		Message  string          `json:"message"`
	}
	if json.Unmarshal(data, &env) != nil {
		return nil, &Error{Kind: Classify(resp.StatusCode, string(data)), Status: resp.StatusCode, Msg: truncate(string(data), 300)}
	}
	if len(env.Response) > 0 {
		var inner struct {
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		}
		_ = json.Unmarshal(env.Response, &inner)
		if inner.Error != nil {
			return nil, c.threeDReject(a, resp.StatusCode, inner.Error.Code, inner.Error.Code+": "+inner.Error.Message)
		}
		if resp.StatusCode >= 400 {
			return nil, c.threeDReject(a, resp.StatusCode, "", string(env.Response))
		}
		return env.Response, nil
	}
	if resp.StatusCode >= 400 || env.Error != nil {
		return nil, c.threeDReject(a, resp.StatusCode, "", env.Message)
	}
	return json.RawMessage(data), nil
}

// threeDReject 上游拒绝本次 3D 请求。HTTP 401/403 或腾讯 AuthFailure 族业务码说明
// 手里的临时凭证已失效：丢掉该账号的自动签发缓存，下一次取凭证时重新签发。
func (c *Client) threeDReject(a *auth.Auth, status int, code, msg string) *Error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden || strings.HasPrefix(code, "AuthFailure") {
		if inv, ok := c.ThreeDCredential.(interface{ Invalidate(uid string) }); ok && a != nil {
			inv.Invalidate(a.UID)
		}
	}
	return threeDError(status, msg)
}

// threeDTempToken 取本次签名用的 `ck_t_` 临时凭证。未挂载实现时返回 ErrThreeDNoCredential。
func (c *Client) threeDTempToken(ctx context.Context, a *auth.Auth) (string, error) {
	if c.ThreeDCredential == nil {
		return "", ErrThreeDNoCredential
	}
	tok, err := c.ThreeDCredential.TempToken(ctx, a)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(tok) == "" {
		return "", ErrThreeDNoCredential
	}
	return tok, nil
}

// FetchClientTempKey 自动签发：拿账号自己的 access token 调官方 /v2/api-keys 换一份
// `ck_t_` 临时云凭证（官方客户端 connect_cloud_service 的同款请求，5.9.3 提取 +
// 2026-11-14 真机验证）。返回凭证与过期时刻；expires_at 缺失或格式未知时返回零值，
// 由调用方按兜底 TTL 处理。失败一律归入 ErrThreeDNoCredential 族——这是网关侧
// 取凭证失败，不该被当成账号健康问题去冷却换号。
//
// 静态 API Key 账号（ck_/pt_ 前缀）没有 OAuth 身份，永远签不出来，直接短路。
func (c *Client) FetchClientTempKey(ctx context.Context, a *auth.Auth) (string, time.Time, error) {
	if a == nil || a.IsAPIKey() {
		return "", time.Time{}, ErrThreeDNoCredential
	}
	if err := c.RefreshTokenIfNeeded(a, 0); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: refresh: %v", ErrThreeDNoCredential, err)
	}
	endpoint := c.chatBase(a) + threeDKeyPath
	payload, err := json.Marshal(map[string]any{
		"name":              threeDKeyName,
		"expire_in_seconds": threeDKeyTTLSeconds,
		"temporary":         true,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrThreeDNoCredential, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrThreeDNoCredential, err)
	}
	// RPC 层形态（与 refresh / console-as 同族）：CommonHeaders + Bearer；企业号
	// 另带 X-Enterprise-Id（官方 connect_cloud_service 对非空 enterpriseId 注入）。
	c.CommonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	env, _, err := c.doJSONRaw(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", ErrThreeDNoCredential, err)
	}
	var data struct {
		Key       string `json:"key"`
		ExpiresAt any    `json:"expires_at"`
	}
	if json.Unmarshal(env.Data, &data) != nil || !strings.HasPrefix(data.Key, "ck_t_") {
		return "", time.Time{}, fmt.Errorf("%w: 签发响应缺少合法的 ck_t_ key", ErrThreeDNoCredential)
	}
	return data.Key, threeDKeyExpiresAt(data.ExpiresAt), nil
}

// threeDKeyExpiresAt 解析签发响应的 expires_at：RFC3339 字符串或 unix 秒/毫秒都认，
// 认不出就返回零值（调用方兜底 14 分钟）。这是时间戳不是敏感值。
func threeDKeyExpiresAt(v any) time.Time {
	switch t := v.(type) {
	case string:
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			return ts
		}
	case float64:
		switch {
		case t > 1e12:
			return time.UnixMilli(int64(t))
		case t > 0:
			return time.Unix(int64(t), 0)
		}
	}
	return time.Time{}
}

func threeDError(status int, msg string) *Error {
	if status < 400 {
		status = http.StatusBadGateway
	}
	kind := Classify(status, msg)
	if kind == ErrNone {
		kind = ErrClient
	}
	// 腾讯业务错误通常在 HTTP 200 信封中返回，不能按 502 冷却健康账号。
	if status == http.StatusBadGateway {
		status = http.StatusBadRequest
	}
	return &Error{Kind: kind, Status: status, Msg: truncate(msg, 500)}
}

func threeDHeaders(host, action string, payload []byte, token string, now time.Time) map[string]string {
	ts := now.Unix()
	date := now.UTC().Format("2006-01-02")
	contentType := "application/json; charset=utf-8"
	canonical := "POST\n/\n\n" +
		"content-type:" + contentType + "\nhost:" + host + "\nx-tc-action:" + strings.ToLower(action) + "\n\n" +
		"content-type;host;x-tc-action\n" + sha256Hex(payload)
	scope := date + "/ai3d/tc3_request"
	toSign := "TC3-HMAC-SHA256\n" + fmt.Sprint(ts) + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	key := threeDHMAC([]byte("TC3codebuddy"), date)
	key = threeDHMAC(key, "ai3d")
	key = threeDHMAC(key, "tc3_request")
	signature := hex.EncodeToString(threeDHMAC(key, toSign))
	return map[string]string{
		"Authorization": "TC3-HMAC-SHA256 Credential=hy-3d." + token + "/" + scope +
			", SignedHeaders=content-type;host;x-tc-action, Signature=" + signature,
		"Content-Type":   contentType,
		"X-TC-Action":    action,
		"X-TC-Version":   "2025-05-13",
		"X-TC-Region":    "ap-guangzhou",
		"X-TC-Timestamp": fmt.Sprint(ts),
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func threeDHMAC(key []byte, text string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(text))
	return h.Sum(nil)
}

// ThreeDState 保留完整任务结果；常见字段另外归一化给 MCP 客户端。
type ThreeDState struct {
	JobID     string
	Status    string
	RequestID string
	ErrorText string
	ResultURL string
	Credit    float64
	Result    json.RawMessage
}

func ParseThreeDState(raw []byte, jobID string) ThreeDState {
	st := ThreeDState{JobID: jobID, Result: append(json.RawMessage(nil), raw...)}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return st
	}
	st.Status, _ = v["Status"].(string)
	st.RequestID, _ = v["RequestId"].(string)
	if id, _ := v["JobId"].(string); id != "" {
		st.JobID = id
	}
	if st.Status == "" {
		switch v["JobStatusCode"] {
		case float64(1):
			st.Status = "QUEUED"
		case float64(2):
			st.Status = "PROCESSING"
		case float64(4):
			st.Status = "FAIL"
		case float64(5):
			st.Status = "DONE"
		}
	}
	st.ErrorText, _ = v["ErrorMessage"].(string)
	st.Credit, _ = v["ResultCreditConsumed"].(float64)
	if files, _ := v["ResultFile3Ds"].([]any); len(files) > 0 {
		for _, f := range files {
			m, _ := f.(map[string]any)
			if u, _ := m["Url"].(string); u != "" && m["Type"] == "GLB" {
				st.ResultURL = u
			}
		}
		if st.ResultURL == "" {
			m, _ := files[0].(map[string]any)
			st.ResultURL, _ = m["Url"].(string)
		}
	}
	if st.ErrorText == "" {
		st.ErrorText, _ = v["JobErrorMsg"].(string)
	}
	for _, k := range []string{"ResultUrl", "ModelUrl", "ResultModelUrl"} {
		if u, _ := v[k].(string); u != "" {
			st.ResultURL = u
			break
		}
	}
	return st
}

func (s ThreeDState) Done() bool      { return s.Status == "DONE" || s.Status == "FAIL" }
func (s ThreeDState) Succeeded() bool { return s.Status == "DONE" }
