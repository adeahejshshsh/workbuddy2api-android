package server

// /v1/embeddings 的总开关（features.embeddings_enabled，缺省关）。
//
// 关着必须回 403 + code=embeddings_disabled，而不是 404：客户端要能区分
// 「网关没开这个功能」与「上游没有这个协议」——后者会让 SDK 把模型名判错。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func postEmbeddings(t *testing.T, live *livecfg.Holder) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(Config{Live: live, Pool: pool.New(""), Upstream: upstream.New()})
	body := `{"model":"bge-m3","input":"hello"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

func TestEmbeddingsDisabledByDefault(t *testing.T) {
	// 空快照 = 未装配 live（等价 features.embeddings_enabled 缺省 false）。
	rec := postEmbeddings(t, livecfg.New(livecfg.Snapshot{}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403（缺省关闭）body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "embeddings_disabled") {
		t.Fatalf("错误码应是 embeddings_disabled：%s", rec.Body)
	}
}

func TestEmbeddingsEnabledPassesGate(t *testing.T) {
	// 打开后不再被 403 拦（没有上游账号 → 后续是 503/其他，总之不是闸门拒绝）。
	rec := postEmbeddings(t, livecfg.New(livecfg.Snapshot{EmbeddingsEnabled: true}))
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "embeddings_disabled") {
		t.Fatalf("打开后仍被闸门拦：%s", rec.Body)
	}
}

func TestEmbeddingsNilLiveUsesStaticConfig(t *testing.T) {
	// Live 为 nil 的裸用路径要回落到构造期静态字段（否则测试/嵌入式用法永远 403）。
	h := NewHandler(Config{EmbeddingsEnabled: true, Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"bge-m3","input":"x"}`))
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "embeddings_disabled") {
		t.Fatalf("静态 EmbeddingsEnabled=true 仍被拦：%s", rec.Body)
	}
}
