// embeddings.go 上游嵌入端点 POST /v2/embeddings（与 chat 同域同头）。
//
// 上游嵌入模型与对话模型是**两套注册表**：/v3/config 的 models 列表里没有嵌入模型，
// 唯一实测可用的是 bge-m3（1024 维）。请求未知模型时上游统一回 500 +
// code=11351「embedding model [x] not found」——这是「模型不存在」而不是「服务坏了」，
// 所以本文件把它单独成 ErrEmbeddingModelNotFound，供 HTTP 层映射成 404。
//
// 响应体是 OpenAI 形状的 {code,msg,model,data[{embedding}],usage}（data 里可能缺
// object/index，由网关补齐——见 server/embeddings.go）。向量是 float 数组，批量
// 1024 维可达 MB 级，故读体上限放到 16 MiB（chat 的 1 MiB 不够）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// EmbeddingsPath 上游嵌入端点路径（与 chat 共用 base，realm 决定 CN / global）。
const EmbeddingsPath = "/v2/embeddings"

// EmbeddingModelBGE 上游唯一实测可用的嵌入模型（1024 维）。仅用于错误文案与文档，
// 请求里仍按客户端给的模型名透传（不替客户端改模型）。
const EmbeddingModelBGE = "bge-m3"

// ErrEmbeddingModelNotFound 上游 11351：请求的嵌入模型不在上游注册表。
// HTTP 层据此回 404（OpenAI 语义），其余上游失败仍按 502/503 处理。
var ErrEmbeddingModelNotFound = errors.New("embedding model not found")

// EmbeddingUsage 上游用量（字段名与 OpenAI 一致，缺失即为 0）。
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// Embeddings 发起一次嵌入请求，返回上游原文 JSON（{code,msg,model,data,usage}）。
//
// input 必须是字符串数组（单条也包成一条数组——上游只认数组形态）。
// ≥400 走 Classify 定 Kind，供调用方复用 chat/images 的失败处置；业务 code 非 0
// 即使 HTTP 200 也当错误返回（不把错误信封当向量透传给客户端）。
func (c *Client) Embeddings(ctx context.Context, a *auth.Auth, model string, input []string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"model": model, "input": input})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+EmbeddingsPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.ChatHeaders(req, a, "", ChatMeta{})
	resp, err := c.do(req) // 本仓 do() 只收 req
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	// 业务信封先解一次：11351 在 500 上也要能识别，故先于状态码判定。
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Code == 11351 {
		return nil, fmt.Errorf("%w: %s", ErrEmbeddingModelNotFound, truncate(env.Msg, 160))
	}
	// 限流/排队的等待口径随信封带出：头优先，6020/6021 的 data.retry_after 兜底
	// （与 chat 链路同口径，见 parseQueueRetryAfter）。缺了它 handler 只能按基数冷却。
	upErr := func() *Error {
		kind := Classify(resp.StatusCode, string(raw))
		if kind == ErrNone {
			kind = ErrClient
		}
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		if d, ok := ParseRetryAfter(resp.Header); ok {
			ue.RetryAfter = d
		} else if kind == ErrUpstreamBusy {
			if d, ok := parseQueueRetryAfter(raw); ok {
				ue.RetryAfter = d
			}
		}
		return ue
	}
	if resp.StatusCode >= 400 {
		return nil, upErr()
	}
	if env.Code != 0 {
		return nil, upErr()
	}
	return json.RawMessage(raw), nil
}
