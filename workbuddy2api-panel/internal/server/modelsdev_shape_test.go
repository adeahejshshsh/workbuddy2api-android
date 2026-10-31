package server

import (
	"reflect"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 这组用例守的是 models.dev 附加声明的三条硬规则：
// 纯追加（既有键一个不变）、同源派生（limit 直接搬运 entry 已有值）、不编造（无源字段不出现）。

// TestModelsDevShapeKeepsOriginalFields 纯追加：shape 前后既有键的值逐一相同，
// 新增键只落在 models.dev 词表内。这条用例就是「原有输出一个字都不改」的机器证明。
func TestModelsDevShapeKeepsOriginalFields(t *testing.T) {
	mi := upstream.ModelInfo{
		ID:                "fast-model",
		Name:              "快速",
		SupportsImages:    true,
		SupportsToolCall:  true,
		SupportsReasoning: true,
	}
	entry := map[string]any{
		"id":             "cn:fast-model",
		"object":         "model",
		"created":        1753600000,
		"owned_by":       "workbuddy",
		"context_length": int64(300000),
	}
	entry = applyModelInfoFields(entry, mi)
	before := make(map[string]any, len(entry))
	for k, v := range entry {
		before[k] = v
	}

	applyModelsDevShape(entry, mi, []string{"medium"}, false)

	for k, want := range before {
		got, ok := entry[k]
		if !ok {
			t.Fatalf("既有键被删除: %s", k)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("既有键被改写: %s = %v want %v", k, got, want)
		}
	}
	allowed := map[string]bool{
		"modalities": true, "limit": true, "tool_call": true,
		"reasoning": true, "reasoning_options": true,
		// magpie 私有方言，见 applyModelsDevShape 末尾：故意超出 models.dev 词表
		"supported_reasoning_levels": true,
	}
	for k := range entry {
		if before[k] == nil && !allowed[k] {
			t.Fatalf("多出非 models.dev 词表键: %s", k)
		}
	}
}

// TestModelsDevShapeModalities 看图为 true 时 input 才带 image；output 恒为 text（对话模型）。
func TestModelsDevShapeModalities(t *testing.T) {
	for _, tc := range []struct {
		name   string
		images bool
		want   []string
	}{
		{"纯文本", false, []string{"text"}},
		{"多模态", true, []string{"text", "image"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := map[string]any{}
			applyModelsDevShape(entry, upstream.ModelInfo{SupportsImages: tc.images}, nil, false)
			mod, ok := entry["modalities"].(map[string]any)
			if !ok {
				t.Fatalf("缺 modalities: %v", entry)
			}
			if got := mod["input"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("modalities.input=%v want %v", got, tc.want)
			}
			if got := mod["output"]; !reflect.DeepEqual(got, []string{"text"}) {
				t.Fatalf("modalities.output=%v want [text]", got)
			}
		})
	}
}

// TestModelsDevShapeLimitSameSource limit 必须直接搬运 entry 已有值（同源派生），
// 且 max_output_tokens 缺失时不能凭 1M/131072 之类的兜底值补一个 output。
func TestModelsDevShapeLimitSameSource(t *testing.T) {
	entry := map[string]any{"context_length": int64(1000000)}
	applyModelsDevShape(entry, upstream.ModelInfo{}, nil, false)

	limit, ok := entry["limit"].(map[string]any)
	if !ok {
		t.Fatalf("缺 limit: %v", entry)
	}
	if got, want := limit["context"], entry["context_length"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("limit.context=%v 与 context_length=%v 不一致", got, want)
	}
	if _, exists := limit["output"]; exists {
		t.Fatalf("上游未下发 max_output_tokens 时不得编造 limit.output: %v", limit)
	}
}

// TestModelsDevShapeReasoningOffFolded "off" 不是思考档，必须折成 toggle；
// 档位 values 与 reasoning_supported_efforts 的语义差异在此固化。
func TestModelsDevShapeReasoningOffFolded(t *testing.T) {
	entry := map[string]any{}
	mi := upstream.ModelInfo{SupportsReasoning: true, CanDisableThinking: true}
	applyModelsDevShape(entry, mi, []string{"low", "high", "max", "off"}, false)

	opts, ok := entry["reasoning_options"].([]map[string]any)
	if !ok || len(opts) != 2 {
		t.Fatalf("reasoning_options 形状不对: %v", entry["reasoning_options"])
	}
	if got := opts[0]["values"]; !reflect.DeepEqual(got, []string{"low", "high", "max"}) {
		t.Fatalf("effort values 应剔除 off: %v", got)
	}
	if got := opts[1]["type"]; got != "toggle" {
		t.Fatalf("应追加 toggle 项: %v", opts)
	}
	if entry["reasoning"] != true {
		t.Fatalf("reasoning 应为 true")
	}
}

// TestModelsDevShapeNoFabrication 无数据源的 models.dev 字段一个都不能出现。
func TestModelsDevShapeNoFabrication(t *testing.T) {
	entry := map[string]any{"context_length": int64(300000)}
	// 刻意不传 efforts：本用例只查「无源字段不出现」，是否声明 reasoning 由能力旗标决定。
	applyModelsDevShape(entry, upstream.ModelInfo{SupportsImages: true, SupportsToolCall: true}, nil, false)

	for _, k := range []string{
		"cost", "attachment", "structured_output", "open_weights",
		"release_date", "knowledge", "temperature", "family",
	} {
		if _, exists := entry[k]; exists {
			t.Fatalf("不得编造无源字段 %s: %v", k, entry[k])
		}
	}
	// 无思考能力时 reasoning/reasoning_options 都不写（缺省即不支持）
	if _, exists := entry["reasoning"]; exists {
		t.Fatalf("无思考能力不得写 reasoning")
	}
	if _, exists := entry["reasoning_options"]; exists {
		t.Fatalf("无档位不得写 reasoning_options")
	}
}

// TestModelsDevShapeMagpieLevels supported_reasoning_levels 是 magpie 的私有方言：
// 只认 EffortRank 的词（none…ultra），所以「关闭思考」要折成 none 而不是 off；
// 出图模型与无档位模型整键不写。
func TestModelsDevShapeMagpieLevels(t *testing.T) {
	t.Run("档位+关思考", func(t *testing.T) {
		entry := map[string]any{}
		mi := upstream.ModelInfo{SupportsReasoning: true}
		applyModelsDevShape(entry, mi, []string{"low", "high", "max", "off"}, false)

		list, ok := entry["supported_reasoning_levels"].([]map[string]string)
		if !ok {
			t.Fatalf("缺 supported_reasoning_levels: %v", entry)
		}
		want := []map[string]string{
			{"effort": "none"}, {"effort": "low"}, {"effort": "high"}, {"effort": "max"},
		}
		if !reflect.DeepEqual(list, want) {
			t.Fatalf("supported_reasoning_levels=%v want %v", list, want)
		}
	})

	t.Run("只有开关", func(t *testing.T) {
		entry := map[string]any{}
		mi := upstream.ModelInfo{SupportsReasoning: true, CanDisableThinking: true}
		applyModelsDevShape(entry, mi, nil, false)

		list, ok := entry["supported_reasoning_levels"].([]map[string]string)
		if !ok || !reflect.DeepEqual(list, []map[string]string{{"effort": "none"}}) {
			t.Fatalf("应只有 none: %v", entry["supported_reasoning_levels"])
		}
	})

	t.Run("无档位不写", func(t *testing.T) {
		entry := map[string]any{}
		applyModelsDevShape(entry, upstream.ModelInfo{SupportsToolCall: true}, nil, false)
		if _, exists := entry["supported_reasoning_levels"]; exists {
			t.Fatalf("无档位不得写 supported_reasoning_levels: %v", entry["supported_reasoning_levels"])
		}
	})

	t.Run("出图模型不写", func(t *testing.T) {
		entry := map[string]any{"image_generation": true}
		applyModelsDevShape(entry, upstream.ModelInfo{}, nil, true)
		if _, exists := entry["supported_reasoning_levels"]; exists {
			t.Fatalf("出图模型不得写 supported_reasoning_levels")
		}
	})
}

// TestModelsDevShapeImageModel 出图模型：output 写 image，且不出现 limit。
func TestModelsDevShapeImageModel(t *testing.T) {
	entry := map[string]any{"id": "cn:hunyuan-image-alpha", "image_generation": true}
	applyModelsDevShape(entry, upstream.ModelInfo{}, nil, true)

	mod, ok := entry["modalities"].(map[string]any)
	if !ok {
		t.Fatalf("缺 modalities: %v", entry)
	}
	if got := mod["output"]; !reflect.DeepEqual(got, []string{"image"}) {
		t.Fatalf("modalities.output=%v want [image]", got)
	}
	if _, exists := entry["limit"]; exists {
		t.Fatalf("出图模型不得有 limit: %v", entry["limit"])
	}
	if _, exists := entry["reasoning_options"]; exists {
		t.Fatalf("出图模型不得有 reasoning_options")
	}
}
