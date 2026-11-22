// speech.go 官方语音能力（协议取自官方桌面端包内 `main/server.js` 的静态反查，
// 2026-11-19，见 issue #277 的协议说明；2026-11-20 真号实测跑通 TTS→ASR 闭环，未抓包对照）：
//
//	POST {chatBase}/v2/audio/speech    文本转语音（SSE，PCM base64 分片）
//	POST {chatBase}/agenttool/v1/asr   语音转文本（multipart 字段 audio）
//
// 与 chat 同域（chatBase，realm 决定 CN/global）同头（ChatHeaders 三件套）。
//
// TTS 的请求体与事件名逐字照抄官方客户端：
//
//	{model:"hunyuan-tts-2.5", stream:true,
//	 speech:{input, voice:"f191_female_cn_tts2.5_flash", response_format:"pcm", sample_rate:24000}}
//
// 回程 SSE 事件 `speech.audio.delta`（audio.data 是 base64 PCM）→ `speech.audio.done`
// → `[DONE]`。官方还有 WebSocket 流式 ASR（/clientcap/v2/asr/stream），本网关不做。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// SpeechPath 文本转语音（SSE）。
	SpeechPath = "/v2/audio/speech"
	// ASRPath 语音转文本（multipart，非流式；流式是 WebSocket，不接）。
	ASRPath = "/agenttool/v1/asr"

	// TTSModel 官方客户端写死的模型（不在 /v1/models 目录里，是独立端点）。
	TTSModel = "hunyuan-tts-2.5"
	// TTSDefaultVoice 官方默认音色；客户端可覆盖。
	TTSDefaultVoice = "f191_female_cn_tts2.5_flash"
	// TTSSampleRate 官方固定 24kHz；回程 PCM 是 s16le 单声道，转 WAV 时必须用它。
	TTSSampleRate = 24000
)

// Speech 合成语音，返回 PCM（s16le 单声道，采样率 TTSSampleRate）。
//
// 为什么返回 PCM 而不是直接返回 WAV：PCM 是上游的原始产物，封装格式属于对外契约，
// 由 server 层按调用方要的 response_format 决定（现在只出 WAV）。
func (c *Client) Speech(ctx context.Context, a *auth.Auth, text, voice string) ([]byte, error) {
	if voice == "" {
		voice = TTSDefaultVoice
	}
	body, err := json.Marshal(map[string]any{
		"model":  TTSModel,
		"stream": true,
		"speech": map[string]any{
			"input":           text,
			"voice":           voice,
			"response_format": "pcm",
			"sample_rate":     TTSSampleRate,
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+SpeechPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.ChatHeaders(req, a, "", ChatMeta{})
	// ChatHeaders 会把 Accept 覆盖成 application/json（chat 的官方口径）。TTS 实测上游
	// 不看 Accept、照样回 SSE，但声明与实收对齐才能让中间层（Caddy/APISIX）不加缓冲。
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.do(req) // 本仓 do() 收 req（代理在 Transport 层，不经此处）
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, upstreamError(resp.StatusCode, raw)
	}
	// SSE 分片可能比默认 64KiB 行长得多（一帧 base64 PCM 就有几十 KB）。
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var pcm []byte
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var ev struct {
			Type  string `json:"type"`
			Audio struct {
				Data string `json:"data"`
			} `json:"audio"`
			Error *struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil, fmt.Errorf("tts stream returned invalid json: %w", err)
		}
		// 上游可以在 SSE 里回错误帧（`{"type":"error","error":{...}}`）；不认它就会一路
		// 掉到 "completed without audio"，把上游的原因咽掉。
		if ev.Error != nil && (ev.Error.Message != "" || ev.Error.Code != nil) {
			return nil, &Error{Kind: Classify(http.StatusBadGateway, payload), Status: http.StatusBadGateway,
				Msg: fmt.Sprintf("tts stream error: %s", truncate(ev.Error.Message, 160))}
		}
		switch ev.Type {
		case "speech.audio.delta":
			chunk, derr := base64.StdEncoding.DecodeString(ev.Audio.Data)
			if derr != nil {
				return nil, fmt.Errorf("tts audio delta is not base64: %w", derr)
			}
			pcm = append(pcm, chunk...)
		case "speech.audio.done":
			// 官方实现要求 done 之后还有 [DONE]，但两者都在时已经拿全音频，直接收尾。
			return pcm, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read tts stream: %w", err)
	}
	if len(pcm) == 0 {
		return nil, fmt.Errorf("tts stream completed without audio")
	}
	// 走到这里说明没见到 speech.audio.done：上游 SSE 被提前切断（2026-11-20 实测
	// APISIX 分块流偶发在 delta 中途 close），pcm 只是半段音频。当成功返回会让客户端
	// 拿到 200 + 残缺录音（issue #277 复现：132480 vs 136320 字节，识别成乱码）。
	return nil, fmt.Errorf("tts stream truncated after %d bytes: no speech.audio.done", len(pcm))
}

// Transcribe 语音转文本，返回上游识别文本。
//
// 上游要 multipart 字段 `audio`（带文件名与 mime），回 {code,text}；code 非 0 即失败
// （与官方客户端 speechToText 的判定一致）。
func (c *Client) Transcribe(ctx context.Context, a *auth.Auth, filename, contentType string, audio []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename == "" {
		filename = "audio.wav"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	part, err := mw.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="audio"; filename="` + filename + `"`},
		"Content-Type":        {contentType},
	})
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audio); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+ASRPath, &buf)
	if err != nil {
		return "", err
	}
	c.ChatHeaders(req, a, "", ChatMeta{})
	// CommonHeaders 会给所有请求写死 Content-Type: application/json，multipart 的 boundary
	// 被它抹掉后上游只能报 15052「failed to parse multipart form」——必须在它之后再设回来。
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.do(req) // 本仓 do() 收 req（代理在 Transport 层，不经此处）
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", upstreamError(resp.StatusCode, raw)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("asr response is not json: %w", err)
	}
	if env.Code != 0 {
		// 把业务码按 CodeMarker 认的形态一起交给 Classify：有些码（15053）只能靠码本身判。
		mark := fmt.Sprintf(`"code":%d %s`, env.Code, env.Msg)
		return "", &Error{Kind: Classify(resp.StatusCode, mark), Status: resp.StatusCode,
			Msg: fmt.Sprintf("asr code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Text, nil
}

// upstreamError 把 ≥400 的响应体翻成带权威分类的 *Error（与 agentTool 同口径）。
func upstreamError(status int, raw []byte) error {
	kind := Classify(status, string(raw))
	if kind == ErrNone {
		kind = ErrClient
	}
	return &Error{Kind: kind, Status: status, Msg: truncate(string(raw), 200)}
}
