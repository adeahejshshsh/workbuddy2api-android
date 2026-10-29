// webagent.go 国际版「网页通道」每日活跃：把一条 console/as 云 agent 会话真正跑起来。
//
// 为什么需要（2026-10-29 实测 + issue #111 复核）：国际版官方每日活跃奖励只认
// **跑完的网页 agent 会话**：
//
//   - 桌面身分发 /v2/chat/completions：不计数（能点亮 growth 连登，但拿不到每日积分）；
//   - 只发 /v2/report：同样不计数（同上，连登会涨、积分不动）；
//   - 只建会话不接沙箱：会话永远停在 CREATING/working，零输出，也不算一次有效对话
//     （上游 issue #90 抓到的就是这个坑）。
//
// 网页端真实链路（本文件实现的全部）：
//
//	POST {base}/console/as/conversations/         建会话 → data.id（只是排队）
//	GET  .../{id}/session                         取沙箱 link / token / sessionId / cwd
//	GET  <link>  Accept: text/event-stream        SSE 通道，响应头 Acp-Connection-Id
//	POST <link>  Acp-Connection-Id: <id>          JSON-RPC：initialize → session/load
//	                                              → session/set_model → session/prompt
//	GET  .../{id}                                 轮询 data.status 到 completed
//	POST .../{id}/stop（失败再 /delete）           收尾，释放上游的活跃会话名额
//
// 三个**必须照抄客户端**的细节，每一条写错都会静默失效：
//
//  1. ACP 的 Authorization 用 /session 返回的**沙箱 token**，不是账号的 access token。
//     用账号 token 一律 401 `12801:鉴权失败`——SSE 那一步照样 200 并给出
//     Acp-Connection-Id，只有真正的 JSON-RPC 请求才暴露鉴权错，很容易被误判成
//     「链路搭好了只是没输出」。
//  2. ACP 用**浏览器**身份（Chromium UA + Origin/Referer /app），不是桌面端
//     WorkBuddy UA 指纹；建会话/取 session 这两个 console 接口同样是 web 身份。
//  3. 上游对每个账号同时存在的活跃会话有硬上限（10 条，超了 429/code 10105）。
//     每条会话跑完必须 stop/delete，否则每天漏一条，攒满就再也建不出会话。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// webConversationsPath console/as 会话集合（结尾斜杠是上游要求，缺了会 404）。
	webConversationsPath = "/console/as/conversations/"
	// webConversationOrigin 建会话时必须带的口径值：网页端 app 身份。
	// 桌面端自己建的是 legacy_workbuddy_local（本机会话），两者在上游是不同通道。
	webConversationOrigin = "workbuddy-app"
	// webDailyModel 每日活跃用的模型：deepseek-v4.1-flash 在国际版倍率 x0.00
	// （2026-10-29 目录实测），跑一轮不扣积分——打卡不能自己烧钱。
	webDailyModel = "deepseek-v4.1-flash"
	// webDailyPrompt 会话内容。取最短的寒暄：网页端历史里留着它也不刺眼。
	webDailyPrompt = "Hi"
	// webActiveConversationLimit 上游单账号活跃会话硬上限（超限 429/code 10105）。
	webActiveConversationLimit = 10
	// webPollInterval 轮询会话状态的间隔（实测一轮「Hi」约 7 秒跑完）。
	webPollInterval = 3 * time.Second
	// webTurnTimeout 单轮从建会话到 completed 的总预算。
	webTurnTimeout = 120 * time.Second
	// webSessionWait 等沙箱 link/token 就绪的上限：刚建会话时会话状态可能还是
	// CREATING，/session 偶尔先回空 link，重试几次比直接判失败更接近真实时序。
	webSessionWait = 15 * time.Second
)

// acpProtocolVersion ACP 协议版本（客户端 initialize 报 1）。
const acpProtocolVersion = 1

// WebSession 一条网页会话的沙箱接入信息（GET .../{id}/session 的 data）。
type WebSession struct {
	Link      string `json:"link"`
	Token     string `json:"token"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// WebTurnResult 一轮网页会话的执行结果（供调度器落日志）。
type WebTurnResult struct {
	Conversation string
	Status       string
	Events       int // session/update 通知条数
	Chunks       int // agent_message_chunk 段数（>0 = 真的说了话）
	Elapsed      time.Duration
	// SetModelFailed session/set_model 失败：本轮仍跑完了，但用的不是请求的模型。
	// 单独记一个旗标而不是把 Events 弄成负数——调用方要能同时读事件数与这个状态。
	SetModelFailed bool
}

// webReq 构造 console/as 网页通道请求：浏览器身份 + Bearer + X-User-Id。
//
// 不复用 CommonHeaders：那一套是**桌面端 RPC 层**形态（WorkBuddy UA、X-Requested-With、
// 无 Origin）。网页通道在上游是另一个前端，UA / Origin / Referer 都不同，混用会把
// 两个前端指纹糊成一个（两边都不像）。
func (c *Client) webReq(ctx context.Context, a *auth.Auth, method, url string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	base := c.chatBase(a)
	req.Header.Set("User-Agent", globalWebUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/app")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// webConversationsURL 会话集合 URL（base 按 realm：国际版 workbuddy.ai / codebuddy.ai，
// 国内版 workbuddy.cn——CN 也有同一套 console/as 端点）。
func (c *Client) webConversationsURL(a *auth.Auth) string {
	return c.chatBase(a) + webConversationsPath
}

// webJSON 发一次网页通道 JSON 请求并把信封解到 out（可为 nil）。
// 非 2xx 与业务 code!=0 都返回 *Error，错误里带上游 message 便于定位 phase。
func (c *Client) webJSON(ctx context.Context, a *auth.Auth, method, url string, body any, out any) error {
	req, err := c.webReq(ctx, a, method, url, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 网页通道的失败**不进 pool 冷却状态机**：它是每日活跃的旁路调用，与 chat
		// 通道的账号健康无关（同 /v1/models 拉取失败不 NoteError 的理由）。故这里用
		// 普通 error 而不是 *Error——后者会被上层当成账号级信号去做冷却/禁号。
		return fmt.Errorf("web %s %s: HTTP %d: %s", method, url, resp.StatusCode, truncate(string(raw), 160))
	}
	if out == nil {
		return nil
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("web %s %s parse: %w", method, url, err)
	}
	if env.Code != 0 {
		return fmt.Errorf("web %s %s: code=%d msg=%s", method, url, env.Code, env.Msg)
	}
	if len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// CreateWebConversation 建一条网页会话，返回会话 id（上游只**排队**，不代表已跑）。
//
// 请求体照抄网页端：prompt + model + conversationOrigin + plugins（weixinpay 是网页端
// 建会话时固定带的内置插件，缺了不影响跑但形态与客户端不一致）。model 只写进会话
// manifest，真正决定这一轮用哪个模型的是后面的 session/set_model。
func (c *Client) CreateWebConversation(ctx context.Context, a *auth.Auth, prompt, model string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = webDailyPrompt
	}
	if strings.TrimSpace(model) == "" {
		model = webDailyModel
	}
	body := map[string]any{
		"prompt":             prompt,
		"model":              model,
		"conversationOrigin": webConversationOrigin,
		"plugins":            []map[string]string{{"name": "weixinpay", "marketplace": "codebuddy-builtin"}},
	}
	var data struct {
		ID string `json:"id"`
	}
	if err := c.webJSON(ctx, a, http.MethodPost, c.webConversationsURL(a), body, &data); err != nil {
		return "", err
	}
	if strings.TrimSpace(data.ID) == "" {
		return "", fmt.Errorf("web conversation create: 上游未返回会话 id")
	}
	return data.ID, nil
}

// WebConversationSession 取沙箱接入信息；link 为空时短暂重试（刚建会话时上游
// 会话状态可能仍是 CREATING，沙箱还没分配）。
func (c *Client) WebConversationSession(ctx context.Context, a *auth.Auth, id string) (WebSession, error) {
	url := c.webConversationsURL(a) + id + "/session"
	deadline := time.Now().Add(webSessionWait)
	var last WebSession
	for {
		var s WebSession
		err := c.webJSON(ctx, a, http.MethodGet, url, nil, &s)
		if err == nil && s.Link != "" && s.Token != "" {
			return s, nil
		}
		if err == nil {
			last = s
			err = fmt.Errorf("沙箱未就绪（link/token 为空）")
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("web session: %w", err)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// WebConversationStatus 读会话当前状态（completed = 这一轮真的跑完了）。
func (c *Client) WebConversationStatus(ctx context.Context, a *auth.Auth, id string) (string, error) {
	var data struct {
		Status string `json:"status"`
	}
	if err := c.webJSON(ctx, a, http.MethodGet, c.webConversationsURL(a)+id, nil, &data); err != nil {
		return "", err
	}
	return data.Status, nil
}

// webConversationAction 对会话执行收尾动作（stop / delete）。
//
// 收尾失败不上抛：一轮已经跑完的活跃不能因为清理失败被改判成失败。上游对同一
// 会话重复 stop 是幂等的（状态已经是 completed 也回 code 0）。
func (c *Client) webConversationAction(ctx context.Context, a *auth.Auth, id, action string) error {
	if id == "" {
		return nil
	}
	url := c.webConversationsURL(a) + id + "/" + action
	return c.webJSON(ctx, a, http.MethodPost, url, map[string]any{}, nil)
}

// StopWebConversation 结束会话（状态 working → completed，释放活跃名额）。
func (c *Client) StopWebConversation(ctx context.Context, a *auth.Auth, id string) error {
	return c.webConversationAction(ctx, a, id, "stop")
}

// DeleteWebConversation 删掉会话（失败轮次没保留价值，免得网页端历史堆满「Hi」）。
func (c *Client) DeleteWebConversation(ctx context.Context, a *auth.Auth, id string) error {
	return c.webConversationAction(ctx, a, id, "delete")
}

// RunWebAgentTurn 接上沙箱把这一轮跑完：ACP 四步 + 轮询状态到 completed。
//
// 只有「status 变成 completed」才算成功——SSE 收到 session/endTurn 也在里面判一次
// 提前收工的依据，但最终以 console 状态为准（hub 的判据同口径：只建会话不接沙箱、
// 或接了沙箱没等它跑完，上游都不计为一次有效对话）。
func (c *Client) RunWebAgentTurn(ctx context.Context, a *auth.Auth, id string, sess WebSession, prompt, model string) (WebTurnResult, error) {
	res := WebTurnResult{Conversation: id}
	started := time.Now()
	if strings.TrimSpace(prompt) == "" {
		prompt = webDailyPrompt
	}
	if strings.TrimSpace(model) == "" {
		model = webDailyModel
	}
	if sess.SessionID == "" {
		sess.SessionID = id
	}
	if sess.Cwd == "" {
		sess.Cwd = "/workspace"
	}

	// SSE 通道：用 chatHTTP（无总时长上限，共享连接池）。首字节前由 ctx 约束。
	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sess.Link, nil)
	if err != nil {
		return res, err
	}
	streamReq.Header.Set("Accept", "text/event-stream")
	streamReq.Header.Set("Authorization", "Bearer "+sess.Token)
	streamReq.Header.Set("User-Agent", globalWebUA)
	stream, err := c.chatHTTP().Do(streamReq)
	if err != nil {
		return res, fmt.Errorf("acp sse: %w", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		return res, fmt.Errorf("acp sse: HTTP %d", stream.StatusCode)
	}
	connID := stream.Header.Get("Acp-Connection-Id")
	if connID == "" {
		return res, fmt.Errorf("acp sse: 响应头没有 Acp-Connection-Id")
	}

	// 读 SSE：只统计，不解析 update 内容（打卡不关心说了什么）。
	// 流结束（或 ctx 取消）时 goroutine 自然退出，不阻塞主流程。
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stream.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var msg struct {
				Method string `json:"method"`
				Params struct {
					Update struct {
						SessionUpdate string `json:"sessionUpdate"`
					} `json:"update"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(payload), &msg) != nil {
				continue
			}
			switch msg.Method {
			case "session/update":
				res.Events++
				if msg.Params.Update.SessionUpdate == "agent_message_chunk" {
					res.Chunks++
				}
			case "session/endTurn":
				res.Events++
			}
		}
	}()

	rpc := func(rid int, method string, params any) error {
		return c.acpPost(ctx, sess, connID, rid, method, params)
	}
	if err := rpc(1, "initialize", map[string]any{
		"protocolVersion": acpProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}); err != nil {
		return res, fmt.Errorf("acp initialize: %w", err)
	}
	if err := rpc(2, "session/load", map[string]any{
		"sessionId": sess.SessionID, "cwd": sess.Cwd, "mcpServers": []any{},
	}); err != nil {
		return res, fmt.Errorf("acp session/load: %w", err)
	}
	// session/set_model 是 ACP 扩展方法，**非致命**：失败时这一轮仍会跑完，只是
	// 可能落到账号默认档（国际版默认 Auto，按付费模型计费）——所以失败要能看到
	// 日志里的原因，但不能因此把整轮判失败。
	if err := rpc(3, "session/set_model", map[string]any{"sessionId": sess.SessionID, "modelId": model}); err != nil {
		res.SetModelFailed = true
	}
	if err := rpc(4, "session/prompt", map[string]any{
		"sessionId": sess.SessionID,
		"prompt":    []map[string]string{{"type": "text", "text": prompt}},
	}); err != nil {
		return res, fmt.Errorf("acp session/prompt: %w", err)
	}

	// 轮询到 completed：上游跑完这一轮才会把状态从 working 改掉。
	deadline := time.Now().Add(webTurnTimeout)
	for {
		st, err := c.WebConversationStatus(ctx, a, id)
		if err == nil {
			res.Status = st
			switch strings.ToLower(st) {
			case "completed":
				res.Elapsed = time.Since(started)
				return res, nil
			case "failed", "error", "deleted":
				res.Elapsed = time.Since(started)
				return res, fmt.Errorf("web conversation status=%s", st)
			}
		}
		if time.Now().After(deadline) {
			res.Elapsed = time.Since(started)
			return res, fmt.Errorf("web conversation 在 %s 内没有跑完（status=%s）", webTurnTimeout, res.Status)
		}
		select {
		case <-ctx.Done():
			res.Elapsed = time.Since(started)
			return res, ctx.Err()
		case <-time.After(webPollInterval):
		}
	}
}

// acpPost 发一条 ACP JSON-RPC 请求。
//
// 注意 Authorization 用的是**沙箱 token**（sess.Token），不是账号 access token：
// 用账号 token 会 401 `12801:鉴权失败`，而 SSE 通道照样 200——这是本实现第一次
// 落地时踩的坑（见文件头第 1 条）。
func (c *Client) acpPost(ctx context.Context, sess WebSession, connID string, rid int, method string, params any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sess.Link, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Acp-Connection-Id", connID)
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	req.Header.Set("User-Agent", globalWebUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 120))
	}
	return nil
}
