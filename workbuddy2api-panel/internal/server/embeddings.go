// embeddings.go POST /v1/embeddings：OpenAI 兼容嵌入端点（实验特性，默认关闭）。
//
// 上游嵌入与对话是两套注册表（/v3/config 不含嵌入模型），实测唯一可用的是 bge-m3
// （1024 维）。因此本端点：
//   - 默认关闭：关着时回 403 embeddings_disabled（不是 404——功能存在，只是没开）。
//     开关是 features.embeddings_enabled，面板保存后热生效（livecfg 快照）。
//   - 模型名原样透传给上游，不替客户端改模型；未知模型由上游回 11351，本端映射成
//     OpenAI 语义的 404 model_not_found，并在文案里点出已知可用模型。
//   - 回程把上游 data[] 补齐 OpenAI 契约（object/index）；条数与输入不符时宁可不返回，
//     也不能错位配对（向量错位比报错更难查）。
//   - 取号与失败处置与 /v1/search、/v1/images 同一套：单次取号、换号上限 3、
//     请求级错误（参数/内容/模型）fail-fast 不烧全池。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// embeddingsAttempts 失败换号上限（与 search/images 同量级）。
const embeddingsAttempts = 3

type embeddingsRequest struct {
	Model string `json:"model"`
	// Input 支持 OpenAI 的两种常用形态：单条字符串 / 字符串数组。
	// 分词数组（[][]int）不接——上游只认文本，转发无意义。
	Input any `json:"input"`
	// Realm 可选，直接指定取号域（优先于 model 前缀）。
	Realm string `json:"realm"`
}

// normalizeEmbeddingInput 把 input 归一成字符串数组。
// 空串与空数组一律报错：上游对空输入没有有意义的行为，交给它只会换来一条难懂的 500。
func normalizeEmbeddingInput(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, errors.New("缺少 input 字段")
	case string:
		if t == "" {
			return nil, errors.New("input 不能为空")
		}
		return []string{t}, nil
	case []any:
		if len(t) == 0 {
			return nil, errors.New("input 不能为空数组")
		}
		out := make([]string, 0, len(t))
		for i, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("input[%d] 必须是字符串", i)
			}
			if s == "" {
				return nil, fmt.Errorf("input[%d] 不能为空串", i)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, errors.New("input 只支持字符串或字符串数组")
	}
}

// normalizeEmbeddingsResponse 把上游原文补成 OpenAI 契约：
// data[i] 必须有 object="embedding" 与下标记；顶层 object="list"。
// 上游缺 index 时按数组顺序补（上游返回顺序即输入顺序，这是 OpenAI 的约定）。
// 条数与输入数不符时返回错误——错位配对比报错更难查。
func normalizeEmbeddingsResponse(raw []byte, want int) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("上游响应不是 JSON: %w", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(obj["data"], &items); err != nil {
		return nil, errors.New("上游响应缺少 data 向量数组")
	}
	if len(items) != want {
		return nil, fmt.Errorf("上游返回 %d 条向量，与输入 %d 条不符", len(items), want)
	}
	out := make([]map[string]any, len(items))
	seen := make(map[int]bool, len(items))
	for i, it := range items {
		vector, ok := it["embedding"].([]any)
		if !ok || len(vector) == 0 {
			return nil, fmt.Errorf("上游 data[%d] 缺少非空数值向量", i)
		}
		for _, value := range vector {
			if _, ok := value.(float64); !ok {
				return nil, fmt.Errorf("上游 data[%d] 向量含非数值元素", i)
			}
		}
		index := i
		if value, present := it["index"]; present {
			n, ok := value.(float64)
			if !ok || n < 0 || n >= float64(want) || math.Trunc(n) != n {
				return nil, fmt.Errorf("上游 data[%d] 下标无效", i)
			}
			index = int(n)
		}
		if seen[index] {
			return nil, fmt.Errorf("上游向量下标 %d 重复", index)
		}
		seen[index] = true
		it["object"] = "embedding"
		it["index"] = index
		out[i] = it
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	obj["data"] = data
	obj["object"] = json.RawMessage(`"list"`)
	// 其余字段（model / usage / 未来新增）原样保留。
	return json.Marshal(obj)
}

func (h *Handler) embeddings(w http.ResponseWriter, r *http.Request) {
	// 本仓不单独设 embeddings 开关：路由挂了就可用（此前这里是一个 `if !true`
	// 死分支，注释还指向一个不存在的 features.embeddings_enabled —— README 也照抄了，
	// 运维会去 config.json 找一个永远不生效的键）。
	raw, err := io.ReadAll(io.LimitReader(r.Body, (8<<20)+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(raw) > 8<<20 {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request", "request body exceeds 8 MiB")
		return
	}
	var req embeddingsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	inputs, err := normalizeEmbeddingInput(req.Input)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 域：realm 显式优先，其次模型自带前缀（cn: / global:），都没有 → cn。
	// 与对话不同，**剥掉**前缀再发给上游：上游目录里的模型 id 不带域前缀（bge-m3）。
	realm, bare := resolveModel(model)
	if req.Realm != "" {
		realm = req.Realm
	}
	if realm != "global" {
		realm = "cn"
	}
	if bare == "" {
		bare = model
	}
	if !h.modelAllowed(realm, bare) || h.modelHidden(realm, bare) {
		writeOpenAIError(w, http.StatusBadRequest, "model_unavailable", "model is unavailable by gateway configuration, try another model")
		return
	}

	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < embeddingsAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		data, err := h.cfg.Upstream.Embeddings(r.Context(), a, bare, inputs)
		if err == nil {
			out, nerr := normalizeEmbeddingsResponse(data, len(inputs))
			if nerr != nil {
				// 形状不对是上游的问题，但换号大概率一样：直接 502，不烧池。
				log.Printf("embeddings %s: 响应形状异常: %v", logfmt.Label(a.UID, a.Nickname), nerr)
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", nerr.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return
		}
		lastErr = err
		// 本仓没有代理池（代理由 proxytunnel 接管，失败形态是 net 错误而非池错误），
		// 故这里不做「代理故障即停」的短路。
		// 模型不存在：换号无意义（注册表是按模型不是按账号），且要给出 OpenAI 的 404 语义。
		if errors.Is(err, upstream.ErrEmbeddingModelNotFound) {
			writeOpenAIError(w, http.StatusNotFound, "model_not_found",
				fmt.Sprintf("嵌入模型 %q 不存在，当前上游嵌入模型仅支持 %q", bare, upstream.EmbeddingModelBGE))
			return
		}
		var ue *upstream.Error
		if errors.As(err, &ue) {
			// 6020/6021 渠道级队列：上游对本形态本身回 429（6020 带 data.retry_after）。
			// 与 chat 链路同口径（PR #108）：429 + Retry-After，不换号（全局队列，换谁都撞），
			// 也不罚号（applyErrorPolicy 的 ErrUpstreamBusy 分支零动作）。
			if ue.Kind == upstream.ErrUpstreamBusy {
				if ue.RetryAfter > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(ue.RetryAfter.Seconds()))))
				}
				writeOpenAIError(w, http.StatusTooManyRequests, "upstream_busy", ue.Msg)
				return
			}
			h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "", ue)
			// 请求级错误（参数/内容/模型权限）：换号必然同样被拒，fail-fast 保住池子。
			if requestLevelFailFastKind(ue.Kind) {
				status := ue.Status
				if status < 400 {
					status = http.StatusBadRequest
				}
				log.Printf("embeddings realm=%s: 请求级错误 kind=%v，不再换号", realm, ue.Kind)
				writeOpenAIError(w, status, "upstream_error", ue.Msg)
				return
			}
		}
		log.Printf("embeddings %s realm=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), realm, attempt+1, err)
	}

	status, code := http.StatusBadGateway, "upstream_error"
	var ue *upstream.Error
	switch {
	case len(tried) == 0:
		status, code = http.StatusServiceUnavailable, "no_account"
		lastErr = fmt.Errorf("没有可用的 %s 账号", realm)
	case errors.As(lastErr, &ue) && ue.Status >= 400:
		status = ue.Status
	}
	log.Printf("embeddings realm=%s: 全部尝试失败（%d 个账号）：%v", realm, len(tried), lastErr)
	writeOpenAIError(w, status, code, lastErr.Error())
}
