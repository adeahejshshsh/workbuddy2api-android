// audio.go 语音端点（实验特性，默认关闭）：
//
//	POST /v1/audio/speech          文本转语音 → WAV（24kHz 单声道 s16le）
//	POST /v1/audio/transcriptions  语音转文本（multipart，字段 file 或 audio）
//
// 上游的两个端点不在 /v1/models 目录里（是独立端点，不是模型），协议来自官方桌面端
// 包内 `main/server.js` 的静态反查 + 真号跑通（issue #277）：`/v2/audio/speech`（SSE + PCM）与
// `/agenttool/v1/asr`（multipart）。**未做真机抓包对照**，故默认关闭：关着时回 403
// （功能存在、只是没开），不是 404。
//
// 取号与失败处置与 /v1/search、/v1/embeddings 同一套：单次取号、换号上限 3、请求级
// 错误 fail-fast 不烧全池。
package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// audioAttempts 失败换号上限（与 search/embeddings 同量级）。
const audioAttempts = 3

// maxAudioBytes 上游 ASR 的硬上限 3MB（2026-11-20 实测：2.9MB 通过，3.1MB 回
// 15052 "ensure file size is under 3MB"）。在入口挡掉，比让上游判 multipart 解析失败干净。
const maxAudioBytes = 3 << 20

// audioBodySlack multipart 分帧（boundary + 头字段）的余量。
const audioBodySlack = 64 << 10

type speechRequest struct {
	// Model 只用来取 realm 前缀（OpenAI 客户端填的是 tts-1 之类的占位名，不转发上游：
	// 上游模型是固定的 hunyuan-tts-2.5）。
	Model string `json:"model"`
	// Voice 音色；空则由上游默认（f191_female_cn_tts2.5_flash）。
	Voice string `json:"voice"`
	// Input 待合成文本，必填。
	Input string `json:"input"`
	// ResponseFormat 只支持 wav / pcm：上游给的是 PCM，声称 mp3 等于撒谎。
	ResponseFormat string `json:"response_format"`
	// Realm 可选，直接指定取号域（优先于 model 前缀）。
	Realm string `json:"realm"`
}

// wavFromPCM 给裸 PCM 套 44 字节 RIFF 头（24kHz 单声道 16-bit）。
func wavFromPCM(pcm []byte, sampleRate int) []byte {
	const ch, bits = 1, 16
	byteRate := sampleRate * ch * bits / 8
	blockAlign := ch * bits / 8
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16)) // fmt 块长度
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))  // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(ch))
	_ = binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&b, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&b, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&b, binary.LittleEndian, uint16(bits))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// audioRealm 取号域：realm 显式优先，其次 model 的 cn:/global: 前缀，都没有 → cn。
// （OpenAI 客户端填的 model 是 tts-1 这类占位名，没有前缀就是 cn，不会被当模型转发。）
func audioRealm(reqRealm, model string) string {
	realm, bare := resolveModel(model)
	if bare == model { // 模型名没有域前缀
		realm = "cn"
	}
	if reqRealm != "" {
		realm = reqRealm
	}
	if realm != "global" {
		realm = "cn"
	}
	return realm
}

// speech POST /v1/audio/speech：OpenAI 形状的文本转语音。
func (h *Handler) speech(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().TTSEnabled {
		writeOpenAIError(w, http.StatusForbidden, "tts_disabled",
			"语音合成未开启：请在面板配置里打开 features.tts_enabled（默认关闭）")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req speechRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	text := strings.TrimSpace(req.Input)
	if text == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "input is required")
		return
	}
	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	switch format {
	case "", "wav", "pcm":
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("response_format %q 不支持：上游只产出 PCM，可选 wav / pcm", req.ResponseFormat))
		return
	}
	realm := audioRealm(req.Realm, req.Model)

	// 记账：与 videos 同一条管道（台账行由 ServeHTTP 的 isChatEntry 接管，这里补
	// 账号/模型/结果）。校验失败的返回不挂 stat——那些是客户端错误、没选号。
	start := time.Now()
	st := &chatStat{start: start, model: upstream.TTSModel, mode: "audio", toks: -1, budget: h.budget}
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		st.requestID = tr.id
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < audioAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		st.uid, st.nick, st.realm, st.attempts = a.UID, a.Nickname, realm, attempt+1
		pcm, err := h.cfg.Upstream.Speech(r.Context(), a, text, req.Voice)
		if err == nil {
			if len(pcm) == 0 {
				st.status, st.outcome = http.StatusBadGateway, reqlog.OutcomeHTTPError
				log.Printf("speech %s: 上游返回空音频", logfmt.Label(a.UID, a.Nickname))
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned no audio")
				return
			}
			out := pcm
			ct := "audio/pcm"
			if format != "pcm" {
				out, ct = wavFromPCM(pcm, upstream.TTSSampleRate), "audio/wav"
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Length", fmt.Sprint(len(out)))
			_, _ = w.Write(out)
			st.status, st.outcome = http.StatusOK, reqlog.OutcomeSuccess
			return
		}
		lastErr = err
		if proxypool.IsError(err) {
			break
		}
		var ue *upstream.Error
		if errors.As(err, &ue) {
			h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "", ue)
			if requestLevelFailFastKind(ue.Kind) {
				status := ue.Status
				if status < 400 {
					status = http.StatusBadRequest
				}
				log.Printf("speech realm=%s: 请求级错误 kind=%v，不再换号", realm, ue.Kind)
				st.status, st.outcome = status, reqlog.OutcomeHTTPError
				writeOpenAIErrorHint(w, status, "upstream_error", ue.Msg,
					upstream.GatewayHint(ue.Kind, ue.Msg, upstream.HintContext{}))
				return
			}
		}
		log.Printf("speech %s realm=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), realm, attempt+1, err)
	}
	st.status, st.outcome = writeAudioFailure(w, "speech", realm, tried, lastErr), reqlog.OutcomeHTTPError
}

// transcriptions POST /v1/audio/transcriptions：OpenAI 形状的语音转文本。
//
// 只收 multipart（OpenAI 的官方形态）；上游同样要 multipart，故音频字节原样转发，
// 不转码——转码只会引入失真与依赖。
func (h *Handler) transcriptions(w http.ResponseWriter, r *http.Request) {
	if !h.loadLive().ASREnabled {
		writeOpenAIError(w, http.StatusForbidden, "asr_disabled",
			"语音识别未开启：请在面板配置里打开 features.asr_enabled（默认关闭）")
		return
	}
	// 先钳住请求体再解析：ParseMultipartForm 会把超过 maxMemory 的部分落到临时文件，
	// 3MB 的限额必须在读盘之前生效，否则一个 500MB 的 body 要先把磁盘填满才被拒。
	// 留 64KiB 余量给 multipart 头与 boundary，超出的部分是畸形请求，不是音频。
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes+audioBodySlack)
	if err := r.ParseMultipartForm(maxAudioBytes); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "audio_too_large",
				fmt.Sprintf("request body exceeds %d bytes; upstream accepts at most 3MB of audio", maxAudioBytes))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"expected multipart/form-data with an audio file: "+err.Error())
		return
	}
	// 超过内存上限的文件部分会被落到 /tmp，不删就每次超限上传漏 3MB 临时文件。
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	file, fh, err := audioPart(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	defer file.Close()
	if fh.Size > maxAudioBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "audio_too_large",
			fmt.Sprintf("audio is %d bytes, upstream accepts at most 3MB", fh.Size))
		return
	}
	audio, err := io.ReadAll(io.LimitReader(file, maxAudioBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read audio: "+err.Error())
		return
	}
	if len(audio) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "audio file is empty")
		return
	}
	realm := audioRealm(r.FormValue("realm"), r.FormValue("model"))
	// OpenAI 的 response_format：json（默认）/ text 可选；srt/vtt/verbose_json 要时间戳，
	// 上游不返回，明确拒掉而不是静默回 json。
	format := strings.ToLower(strings.TrimSpace(r.FormValue("response_format")))
	switch format {
	case "", "json", "text":
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("response_format %q 不支持：上游不返回时间戳，可选 json / text", r.FormValue("response_format")))
		return
	}

	st := &chatStat{start: time.Now(), model: "asr", mode: "audio", toks: -1, budget: h.budget}
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		st.requestID = tr.id
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < audioAttempts; attempt++ {
		a := h.cfg.Pool.PickExcludingForRealm(tried, "", realm)
		if a == nil {
			break
		}
		tried[a.UID] = true
		st.uid, st.nick, st.realm, st.attempts = a.UID, a.Nickname, realm, attempt+1
		text, err := h.cfg.Upstream.Transcribe(r.Context(), a, fh.Filename, fh.Header.Get("Content-Type"), audio)
		if err == nil {
			st.status, st.outcome = http.StatusOK, reqlog.OutcomeSuccess
			if format == "text" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = w.Write([]byte(text))
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"text": text})
			return
		}
		lastErr = err
		if proxypool.IsError(err) {
			break
		}
		var ue *upstream.Error
		if errors.As(err, &ue) {
			h.applyErrorPolicy(a.UID, ue.Kind, ue.Msg, "", ue)
			if requestLevelFailFastKind(ue.Kind) {
				status := ue.Status
				if status < 400 {
					status = http.StatusBadRequest
				}
				log.Printf("transcriptions realm=%s: 请求级错误 kind=%v，不再换号", realm, ue.Kind)
				st.status, st.outcome = status, reqlog.OutcomeHTTPError
				writeOpenAIErrorHint(w, status, "upstream_error", ue.Msg,
					upstream.GatewayHint(ue.Kind, ue.Msg, upstream.HintContext{}))
				return
			}
		}
		log.Printf("transcriptions %s realm=%s attempt=%d: %v", logfmt.Label(a.UID, a.Nickname), realm, attempt+1, err)
	}
	st.status, st.outcome = writeAudioFailure(w, "transcriptions", realm, tried, lastErr), reqlog.OutcomeHTTPError
}

// audioPart 取上传的音频：OpenAI 用 file，官方小程序/其它工具用 audio，两个都认。
func audioPart(r *http.Request) (multipart.File, *multipart.FileHeader, error) {
	for _, field := range []string{"file", "audio"} {
		if f, fh, err := r.FormFile(field); err == nil {
			// 上游按 mime 判格式；客户端不带时按扩展名补一次。
			if fh.Header.Get("Content-Type") == "" {
				if ct := mime.TypeByExtension(filepath.Ext(fh.Filename)); ct != "" {
					fh.Header.Set("Content-Type", ct)
				}
			}
			return f, fh, nil
		}
	}
	return nil, nil, errors.New("缺少音频文件：multipart 字段需为 file（OpenAI）或 audio")
}

// writeAudioFailure 两个端点共用末端映射（与 embeddings 同口径），返回写出的状态码
// 供调用方记账。
func writeAudioFailure(w http.ResponseWriter, name, realm string, tried map[string]bool, lastErr error) int {
	status, code := http.StatusBadGateway, "upstream_error"
	var ue *upstream.Error
	switch {
	case proxypool.IsError(lastErr):
		status, code = http.StatusServiceUnavailable, "proxy_unavailable"
	case len(tried) == 0:
		status, code = http.StatusServiceUnavailable, "no_account"
		lastErr = fmt.Errorf("没有可用的 %s 账号", realm)
	case errors.As(lastErr, &ue) && ue.Status >= 400:
		status = ue.Status
	}
	if lastErr == nil {
		lastErr = errors.New("upstream call failed")
	}
	log.Printf("%s realm=%s: 全部尝试失败（%d 个账号）：%v", name, realm, len(tried), lastErr)
	writeOpenAIError(w, status, code, lastErr.Error())
	return status
}
