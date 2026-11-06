package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 回读名额被占满：/content 等不到就回 503 media_busy（带 Retry-After），b64_json 保留直链。
func TestMediaFetchBusy(t *testing.T) {
	old := mediaBusyWait
	mediaBusyWait = 20 * time.Millisecond
	t.Cleanup(func() { mediaBusyWait = old })
	for i := 0; i < cap(mediaFetchSlots); i++ {
		mediaFetchSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(mediaFetchSlots); i++ {
			<-mediaFetchSlots
		}
	})

	rig := newContentRig(t, serveBytes("video-bytes"))
	rec := getContent(rig.h, videoTestTaskID)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "media_busy") || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("名额满时 /content 应回 503 media_busy，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	wantURLKept(t, postB64Image(context.Background(), t, rig.cdn.URL+"/i.png"))
}
