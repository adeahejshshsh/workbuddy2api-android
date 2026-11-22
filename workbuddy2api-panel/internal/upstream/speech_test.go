package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSpeechAndTranscribeWireFormat 锁住两条语音请求的出站形状。
// 重点是 ASR 的 Content-Type：CommonHeaders 会给所有请求写死 application/json，
// 把 multipart 的 boundary 抹掉后上游只能回 15052「failed to parse multipart form」
// （2026-11-20 真号实测踩到过），所以这里必须按真实客户端身份构造再断言。
func TestSpeechAndTranscribeWireFormat(t *testing.T) {
	var asrCT string
	var asrAudio string
	var ttsAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case SpeechPath:
			ttsAccept = r.Header.Get("Accept")
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"speech.audio.delta\",\"audio\":{\"data\":\"AAEC\"}}\n\n")
			io.WriteString(w, "data: {\"type\":\"speech.audio.done\"}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		case ASRPath:
			asrCT = r.Header.Get("Content-Type")
			mr, err := r.MultipartReader()
			if err != nil {
				t.Errorf("上游收到的不是 multipart: %v (Content-Type=%q)", err, asrCT)
				return
			}
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				if p.FormName() == "audio" {
					b, _ := io.ReadAll(p)
					asrAudio = string(b)
				}
			}
			io.WriteString(w, `{"code":0,"text":"识别结果"}`)
		default:
			t.Errorf("意料之外的路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	pcm, err := c.Speech(context.Background(), a, "你好", "")
	if err != nil {
		t.Fatalf("Speech: %v", err)
	}
	if string(pcm) != "\x00\x01\x02" {
		t.Fatalf("PCM = %q, want %%q 解出的 \\x00\\x01\\x02", pcm)
	}

	text, err := c.Transcribe(context.Background(), a, "a.wav", "audio/wav", []byte("RIFFdata"))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "识别结果" {
		t.Fatalf("text = %q", text)
	}
	if !strings.HasPrefix(asrCT, "multipart/form-data; boundary=") {
		t.Fatalf("ASR Content-Type = %q, 必须是带 boundary 的 multipart", asrCT)
	}
	if asrAudio != "RIFFdata" {
		t.Fatalf("audio 字段 = %q", asrAudio)
	}
	// ChatHeaders 会把 Accept 覆盖成 application/json；TTS 必须在它之后设回来。
	if ttsAccept != "text/event-stream" {
		t.Fatalf("TTS Accept = %q, 必须是 text/event-stream（被 ChatHeaders 覆盖了）", ttsAccept)
	}
}

// TestSpeechSurfacesStreamErrorEvent 上游可以在 SSE 里回错误帧，不认它就会退化成
// "completed without audio"，把上游的原因咽掉（issue #277 反馈）。
func TestSpeechSurfacesStreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"error","error":{"message":"input too long"}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	_, err := c.Speech(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "你好", "")
	if err == nil || !strings.Contains(err.Error(), "input too long") {
		t.Fatalf("SSE 错误帧被咽掉了: %v", err)
	}
}

// TestSpeechRejectsTruncatedStream 上游 SSE 在 delta 中途被切断（没发 speech.audio.done）
// 时不能当成功返回：客户端会拿到 200 + 半段录音（issue #277 实测 132480 vs 136320 字节，
// ASR 识别成乱码）。必须报错让 handler 换号重试。
func TestSpeechRejectsTruncatedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"type":"speech.audio.delta","audio":{"data":"AAEC"}}`+"\n\n")
		// 到此断开：没有 done、没有 [DONE]
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	if _, err := c.Speech(context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, "你好", ""); err == nil {
		t.Fatal("截断的 SSE 被当成成功返回了")
	}
}

// TestASRRecognitionFailureIsRequestLevel 15053 是「这段音频识别不出来」，同一文件
// 换号也是同一句；归请求级才不会为一段坏音频轮转全池。
func TestASRRecognitionFailureIsRequestLevel(t *testing.T) {
	body := `{"code":15053,"msg":"ASR recognition failed"}`
	if got := Classify(502, body); got != ErrBadParams {
		t.Fatalf("Classify(502, 15053) = %v, want ErrBadParams", got)
	}
	if h := GatewayHint(ErrBadParams, body, HintContext{}); !strings.Contains(h, "WAV") {
		t.Fatalf("15053 的 hint 应指向容器格式: %q", h)
	}
}
