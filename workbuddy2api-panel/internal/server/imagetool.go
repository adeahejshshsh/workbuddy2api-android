// imagetool.go Responses 托管的 image_generation 工具执行器。
//
// OpenAI 的 image_generation 是**服务端工具**：客户端只声明 {"type":"image_generation"}，
// 出图由服务方执行，结果以 output item {"type":"image_generation_call","result":<base64>}
// 回报。本网关上游只有 chat-completions 与 /v2/images，所以分三步：
//
//  1. 探测：把 image_generation 当普通 function 送上去（非流式聚合），让**模型自己写
//     提示词**——用户那句「画个适合中秋的电梯广告图」不是给画图模型的提示词；
//  2. 出图：网关走自己的出图管道（imageGenerate，含选号/记账），取字节转 base64；
//  3. 交付：成品作为 image_generation_call 补进回程 output（非流式插 output 头部、
//     流式在 response.created 之后补 output_item.added/done），同时给模型注入一句
//     「图已交付」，让它写配文而不是复述图片。
//
// shortcut: 一个请求只出一张、只探一轮（与 images 端点拒绝 n>1 同口径）；出图 15~20s
// 全在响应开始之前，客户端要靠自己的超时兜住。要多张/多轮再上流内嗅探。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// imageToolName Responses 托管出图工具的固定名（客户端声明里也用它）。
	imageToolName = "image_generation"
	// imageToolMax 单次请求最多出几张（上游一次只出一张，且每张真扣积分）。
	imageToolMax = 1
)

// imageCallItem 成品 → Responses 的 image_generation_call 输出项。result 是 base64
// （OpenAI 就这个形状），status 恒 completed —— 到这一步图已经拿到了。
func imageCallItem(prompt, b64 string) map[string]any {
	return map[string]any{
		"id":             "ig_" + session.NewMessageID(),
		"type":           imageToolName + "_call",
		"status":         "completed",
		"result":         b64,
		"revised_prompt": prompt,
	}
}

// imageResultB64 出图回执 → base64：回 b64_json 的模型直接用，回签名直链的取一次字节。
func imageResultB64(ctx context.Context, raw []byte) (string, error) {
	var env struct {
		Data []struct {
			URL string `json:"url"`
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	if len(env.Data) == 0 {
		return "", errNoImageData
	}
	if env.Data[0].B64 != "" {
		return env.Data[0].B64, nil
	}
	if env.Data[0].URL == "" {
		return "", errNoImageData
	}
	b, err := fetchMedia(ctx, env.Data[0].URL, mediaFetchMax) // 签名直链 12h 有效，取一次就够
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

var errNoImageData = errors.New("出图回执里没有图片")

// imageAugment 探测 → 出图 → 注入。返回 nil body 表示没有可交付的内容（探测失败、
// 模型没打算出图），调用方按普通对话继续；items 是补进回程 output 的输出项。
func (h *Handler) imageAugment(ctx context.Context, chat []byte) ([]byte, []any) {
	var obj map[string]any
	if err := json.Unmarshal(chat, &obj); err != nil {
		return nil, nil
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return nil, nil
	}
	calls, _, _, _, err := h.hostedProbe(ctx, obj, msgs, imageToolName)
	if err != nil {
		log.Printf("WARN: [responses] image_generation 探测失败，降级为普通对话: %v", err)
		return nil, nil
	}
	if len(calls) == 0 {
		return nil, nil // 模型判断这轮不需要出图
	}
	var items []any
	var notes []string
	for i, c := range calls {
		if i >= imageToolMax {
			notes = append(notes, "本次请求只出一张，其余出图请求未执行。")
			break
		}
		var b64 string
		raw, gerr := h.imageGenerate(ctx, map[string]any{"prompt": c.Query})
		if gerr == nil {
			b64, gerr = imageResultB64(ctx, raw)
		}
		if gerr != nil {
			log.Printf("WARN: [responses] image_generation 出图失败: %v", gerr)
			notes = append(notes, "按提示词「"+c.Query+"」出图失败（上游出图接口不可用）。")
			continue
		}
		items = append(items, imageCallItem(c.Query, b64))
		notes = append(notes, "已按提示词「"+c.Query+"」生成 1 张图片。")
	}
	out := map[string]any{}
	for k, v := range obj {
		out[k] = v
	}
	out["messages"] = append(append([]any{}, msgs...), map[string]any{
		"role":    "user",
		"content": imageToolContext(notes, len(items)),
	})
	b, err := json.Marshal(out)
	if err != nil {
		return nil, nil
	}
	return b, items
}

// imageToolContext 注入给模型的说明：图已经由客户端直接显示，别再用文字/base64 复述。
func imageToolContext(notes []string, delivered int) string {
	head := "[出图结果｜由网关代为执行]"
	if delivered > 0 {
		head = "[出图结果｜由网关代为执行] 图片已作为 image_generation_call 输出项交给客户端显示；" +
			"不要再用文字、Markdown 图片语法或 base64 复述图片内容。"
	}
	return head + "\n" + strings.Join(notes, "\n") +
		"\n出图失败就如实告诉用户，不要假装已生成。"
}
