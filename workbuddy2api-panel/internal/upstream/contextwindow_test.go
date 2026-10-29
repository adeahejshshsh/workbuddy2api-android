package upstream

// contextWindow 档位（上游 2026-10-29 `09f2eeb`，5.9.3 新字段）：
// defaultLength 是默认档，supportedLengths 是用户可切换的档位（如 [400000,1000000]）。
// 与 maxInputTokens 不是同一口径 —— 后者是当前生效上限，丢掉档位客户端就发现不了 1M。

import (
	"encoding/json"
	"testing"
)

func TestContextWindowParsed(t *testing.T) {
	raw := `{"id":"gpt-6-astra","name":"Astra","maxInputTokens":400000,"maxOutputTokens":32000,
		"contextWindow":{"defaultLength":400000,"supportedLengths":[400000,1000000]}}`
	var m dynModelEntry
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	mi := m.modelInfo()
	if mi.ContextWindowDefault != 400000 {
		t.Errorf("default=%d want 400000", mi.ContextWindowDefault)
	}
	if len(mi.ContextWindowSupported) != 2 || mi.ContextWindowSupported[1] != 1000000 {
		t.Errorf("supported=%v want [400000 1000000]", mi.ContextWindowSupported)
	}
}

func TestContextWindowAbsentIsZero(t *testing.T) {
	var m dynModelEntry
	if err := json.Unmarshal([]byte(`{"id":"x","maxInputTokens":128000}`), &m); err != nil {
		t.Fatal(err)
	}
	mi := m.modelInfo()
	if mi.ContextWindowDefault != 0 || len(mi.ContextWindowSupported) != 0 {
		t.Errorf("上游没给时不该编造档位: default=%d supported=%v", mi.ContextWindowDefault, mi.ContextWindowSupported)
	}
}
