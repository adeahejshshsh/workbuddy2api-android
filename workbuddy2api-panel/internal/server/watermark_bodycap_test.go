package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// multipart 请求体必须有硬上限（审查 B4）：ParseMultipartForm 的参数只是内存阈值，
// 超出部分落临时文件且总量无界——一个请求能把磁盘打满，之后 state.json 落盘全失败。
func TestWatermarkMultipartBodyIsCapped(t *testing.T) {
	p := pool.New("")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), WatermarkStripEnabled: true})

	// 造一个明显超过上限的 multipart（图片字段声称 40MiB）。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("image", "big.png")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{0x89}, 1<<20)
	for i := 0; i < 40; i++ {
		if _, err := fw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/images/strip-watermark", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 multipart：code=%d want 413（body=%s）", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
