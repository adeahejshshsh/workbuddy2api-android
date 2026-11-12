// report.go growth 域「对话活跃上报」接口：POST {billingBase}/v2/report。
// 照抄客户端 chat_request_send 事件形状（含 conversationId/mode/inputLength 等全字段，
// 勿用最小 3 字段，防上游后续加严）。事件必须带 userId（=账号 uid），缺失则服务端
// 200 但静默丢弃（实测见 REPORT-active-map.md §2）。
//
// 一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务（领养前置）。
// 风控口径：每号每天 1 次即可（activity_hours 单时点），不做多时点高频上报。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// reportPath 活跃上报通道（实测）。
const reportPath = "/v2/report"

// billingJSON 发 billing 域（billingBase，codebuddy.cn）请求并解信封；body 为 nil 时不带请求体。
// 与 travel.go 的 growthJSON 对称（growth 域走 chatBase + BillingHeaders；billing 域走 billingBase）。
// report/checkin 等 billing 端点共用：请求头统一 BillingHeaders，信封与错误语义同 doJSON。
func (c *Client) billingJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	return c.billingJSONCtx(context.Background(), a, method, path, body)
}

// billingJSONCtx 带 ctx 的 billing 请求（bug-12）：此前 ctx 只作用于重试间隔，
// 单次请求仍用无 ctx 的 http.NewRequest + client 120s 超时 —— SIGTERM 时最坏
// 还要等满一次请求才收尾，挤压 run.sh 的 8s 停机预算。
func (c *Client) billingJSONCtx(ctx context.Context, a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	env, _, err := c.billingJSONRawCtx(ctx, a, method, path, body)
	return env.Data, err
}

// billingJSONRawCtx 同 billingJSONCtx，另外把信封与**原始响应体**一并交回。
//
// 为什么需要：daily-checkin 的成功回执是**平铺**的（没有 {code,msg,data} 信封），
// 只交 env.Data 会拿到 null，这次到底发了多少积分就丢了
// （见 checkin_receipt.go 头注释）。
func (c *Client) billingJSONRawCtx(ctx context.Context, a *auth.Auth, method, path string, body any) (apiEnvelope, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return apiEnvelope{}, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.billingBase(a)+path, rdr)
	if err != nil {
		return apiEnvelope{}, nil, err
	}
	c.BillingHeaders(req, a)
	return c.doJSONRaw(req)
}

// billingJSONRaw 无 ctx 版本（签到回执读原始 body 用）。
func (c *Client) billingJSONRaw(a *auth.Auth, method, path string, body any) (apiEnvelope, []byte, error) {
	return c.billingJSONRawCtx(context.Background(), a, method, path, body)
}

// billingMeterJSON 仅对 /billing/meter 族端点（get-user-resource / daily-checkin）
// 按 realm 走双路径 fallback：global 先无 /v2 前缀，ErrNotFound 时二次换有 /v2 前缀
// （上游新旧路径分叉）；cn 单路径（有 /v2）现状不变。仅 global realm 才有多路径。
func (c *Client) billingMeterJSON(a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	return c.billingMeterJSONCtx(context.Background(), a, paths, method, body)
}

// billingMeterJSONCtx 带 ctx（bug-12，语义同 billingJSONCtx）。
func (c *Client) billingMeterJSONCtx(ctx context.Context, a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	env, _, err := c.billingMeterRawCtx(ctx, a, paths, method, body)
	return env.Data, err
}

// billingMeterRaw 同 billingMeterJSON，另外交回信封与原始 body（签到回执要用平铺
// body 里的 credit）。
func (c *Client) billingMeterRaw(a *auth.Auth, paths []string, method string, body any) (apiEnvelope, []byte, error) {
	return c.billingMeterRawCtx(context.Background(), a, paths, method, body)
}

// billingMeterRawCtx 带 ctx 的多路径版本（语义同 billingMeterJSONCtx）。
func (c *Client) billingMeterRawCtx(ctx context.Context, a *auth.Auth, paths []string, method string, body any) (apiEnvelope, []byte, error) {
	var (
		lastErr error
		lastRaw []byte
	)
	for i, path := range paths {
		env, raw, err := c.billingJSONRawCtx(ctx, a, method, path, body)
		if err == nil {
			return env, raw, nil
		}
		lastErr, lastRaw = err, raw
		// 仅 404 换路径（路径不存在才值得 fallback）；其他错误直接返回。
		var ue *Error
		if !errors.As(err, &ue) || ue.Kind != ErrNotFound || i == len(paths)-1 {
			return env, raw, err
		}
	}
	return apiEnvelope{}, lastRaw, lastErr
}

// billingRetryDelay 签到/余额等维护类计费调用瞬时错误重试的间隔基数。
// 独立变量供测试缩短（生产固定 2s：第 1 次重试等 2s、第 2 次等 4s）。
var billingRetryDelay = 2 * time.Second

// isTransientBillingErr 报告 err 是否值得对计费维护类调用做有界重试：
// 上游 5xx（ErrServer，实测偶发 "code 10000 / API request failed with status
// code: 500"）或网络层错误（非 *Error 的传输失败）。业务错误（code!=0 的
// 已签到/参数错、4xx、限流）不重试——重试只会原样再失败一次。
// IsTransientBilling 导出「这次计费类失败值不值得过一会儿再试」的判据，供调度器
// 决定要不要把账号放进延后补签队列（签到本身只有 2s/4s 两次内联重试，覆盖不了
// 上游「请稍后重试」这种需要分钟级沉降的形态）。
func IsTransientBilling(err error) bool { return isTransientBillingErr(err) }

func isTransientBillingErr(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if errors.As(err, &ue) {
		// 5xx（实测偶发 "code 10000 / status 500"）与 429「请求处理中」（上游临时状态，
		// 需要等它结算完）都值得等一会儿再试；其余 4xx 是确定性拒绝。
		return ue.Kind == ErrServer || isBillingProcessingErr(err)
	}
	// 非 *Error = 传输层失败：值不值得重试由调用方决定（签到这类低频维护调用值得）。
	return true
}

// billingProcessingRetryDelays 429「请求处理中」的退避阶梯：这是上游**临时状态**
// （签到/余额查询偶发），比 5xx 需要更久沉降才能结算完；与真·限流的退避分开，
// 免得把限流也拖成三次长重试。
var billingProcessingRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

// billingProcessingMarkers 上游 429 里表示「请求处理中」的文案（中英双形态）。
var billingProcessingMarkers = []string{"请求处理中", "request is being processed", "request processing"}

// isBillingProcessingErr 报告 err 是否为「429 + 请求处理中」形态：临时状态，值得等它沉降。
// 只认带分类的 *Error 且 Status==429；普通 429（真·限流）不匹配，仍不重试。
func isBillingProcessingErr(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(ue.Msg)
	for _, m := range billingProcessingMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// retryBillingTransient 对签到/余额这类低频维护调用做瞬时错误有界重试：
// 最多补打 2 次（间隔 2s、4s），首次成功或非瞬时错误立即返回。chat 热路径
// 不用本策略——它有自己的换号轮转语义，重试会放大在途请求。
func (c *Client) retryBillingTransient(fn func() error) error {
	return c.retryBillingTransientCtx(context.Background(), fn)
}

// retryBillingTransientCtx 可取消版本（bug B14）：SIGTERM 时不必把 2s+4s 睡满，
// 否则优雅停机被拖过 5s 预算，最后一次 Flush 被 run.sh 的强杀夺走。
func (c *Client) retryBillingTransientCtx(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	err := fn()
	if err == nil || !isTransientBillingErr(err) {
		return err
	}
	// 429「请求处理中」比 5xx 需要更久沉降：走 2s/5s/10s 阶梯，其余仍是 2s/4s 两次。
	delays := []time.Duration{billingRetryDelay, 2 * billingRetryDelay}
	if isBillingProcessingErr(err) {
		delays = billingProcessingRetryDelays
	}
	for i := 0; i < len(delays); i++ {
		t := time.NewTimer(delays[i])
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		if err = fn(); err == nil || !isTransientBillingErr(err) {
			return err
		}
	}
	return err
}

// chatRequestEvent 客户端 chat_request_send 事件完整形状（与 probe_active.py chat_event 对齐）。
// userId 为必填字段（= a.UID）；conversationId 由调用方生成，无需真实会话。
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// ReportChatActivity 向上游发送一条对话活跃上报（chat_request_send）。
// conversationID 由调用方生成（如 wb2api-<ms>），无需真实会话——服务端不校验一致性。
// requestID 为本轮请求独立标识（多轮同会话上报时各条不同）；空时回落 conversationID。
// 错误语义与 doJSON 一致：HTTP 非 2xx / 业务 code != 0 → *Error。
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID, requestID string) error {
	return c.ReportChatActivityModel(a, conversationID, requestID, "deepseek-v4-flash", "DeepSeek V4 Flash")
}

// ReportChatActivityModel 同上，但可指定上报携带的模型：供「体验某模型」类任务
// 对齐实际模型（如 Model_chat_GLM5.2 需 requestModelId=glm-5.2 与独立 requestID）。
func (c *Client) ReportChatActivityModel(a *auth.Auth, conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode:             "chat_request_send",
		Timestamp:             now,
		ReportDelay:           0,
		Mode:                  "craft",
		ConversationID:        conversationID,
		RequestID:             requestID,
		InputLength:           12,
		RequestModelID:        modelID,
		RequestModelName:      modelName,
		IsPlan:                false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:          false,
		CodebaseEnable:        false,
		MaxToken:              0,
		MaxSteps:              0,
		Temperature:           0,
		MaxRetries:            0,
		MentionContexts:       []any{},
		KnowledgeID:           []any{},
		KnowledgeName:         []any{},
		CodebaseID:            "",
		MentionContextCount:   0,
		Command:               "",
		ExpertID:              "",
		RecommendID:           "",
		SkillID:               "",
		SkillCount:            0,
		TotalCount:            0,
		FileURI:               "",
		PresentAt:             now,
		TraceID:               "",
		RootRequestID:         requestID,
		ParentConversationID:  conversationID,
		AgentName:             "default",
		AgentType:             "conversation",
		UserID:                a.UID,
	}
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}
	_, err = c.billingJSON(a, http.MethodPost, reportPath, json.RawMessage(raw))
	return err
}

// SetBillingRetryDelayForTest 临时改签到/余额重试的间隔基数，返回恢复函数。
//
// 仅供**其它包的测试**使用：维护类计费调用的重试基数是包内私有变量，
// 上游包自己的测试能直接改，跨包（如 scheduler）改不到 —— 于是一个「上游全 500」
// 的用例会把 1×base + 2×base 真睡满，单测从毫秒级变成十几秒。
func SetBillingRetryDelayForTest(d time.Duration) (restore func()) {
	old := billingRetryDelay
	billingRetryDelay = d
	return func() { billingRetryDelay = old }
}
