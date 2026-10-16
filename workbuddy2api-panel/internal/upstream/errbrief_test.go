package upstream

import (
	"strings"
	"testing"
)

// 用户实际遇到的 body（11158：账号类型不支持该模型）——必须能拆成干净的一句话。
const real11158 = `{"code":11158,"msg":"model [gpt-6-astra] is not allowed for user type [personal_free_user]","requestId":"3c1e0362ddd16c18823d8eab6c8904e5","displayMsg":{"en":"Your account type does not support this model.","zh":"当前账号类型暂不支持该模型。","zh-hant":"目前帳號類型暫不支援此模型。"},"actions":["SUBMIT_FEEDBACK","COPY_ERROR","SWITCH_MODEL"]}`

func TestBriefErrorReal11158(t *testing.T) {
	br := BriefError(real11158)
	if br.Code != "11158" {
		t.Fatalf("code = %q, want 11158", br.Code)
	}
	if br.RequestID != "3c1e0362ddd16c18823d8eab6c8904e5" {
		t.Fatalf("requestId = %q", br.RequestID)
	}
	if br.Display != "当前账号类型暂不支持该模型。" {
		t.Fatalf("display = %q（应优先中文）", br.Display)
	}
	if !strings.Contains(br.Brief, "当前账号类型暂不支持该模型。") {
		t.Fatalf("brief 丢了中文文案: %q", br.Brief)
	}
	if !strings.Contains(br.Brief, "gpt-6-astra") {
		t.Fatalf("brief 丢了模型名: %q", br.Brief)
	}
	if strings.Contains(br.Brief, `\"`) || strings.Contains(br.Brief, `{"code"`) {
		t.Fatalf("brief 仍是转义后的 JSON: %q", br.Brief)
	}
	if !IsModelNotAllowedForUserType(real11158) {
		t.Fatal("11158 未被识别为「账号类型不支持该模型」")
	}
	// 邻居不能误判：11102 是"模型不存在"，语义不同。
	if IsModelNotAllowedForUserType(`{"code":11102,"msg":"service info not found"}`) {
		t.Fatal("11102 被误判成 11158")
	}
	if IsModelNotAllowedForUserType(`{"requestId":"a11158b","code":0}`) {
		t.Fatal("requestId 里含 11158 被误判")
	}
}

func TestBriefErrorVariants(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantCode  string
		contains  string
		notErased bool
	}{
		{"仅 msg", `{"code":6004,"msg":"model rate limit, resets at 12:00"}`, "6004", "model rate limit", true},
		{"嵌套 error 形态", `{"error":{"code":14018,"message":"credits exhausted"}}`, "14018", "credits exhausted", true},
		{"字符串 code", `{"code":"11102","msg":"service info not found"}`, "11102", "service info not found", true},
		{"HTML 错误页", "<html>\n  <body>502 Bad\n Gateway</body>\n</html>", "", "502 Bad Gateway", true},
		{"空体", "", "", "", false},
	}
	for _, c := range cases {
		br := BriefError(c.body)
		if br.Code != c.wantCode {
			t.Errorf("%s: code = %q, want %q", c.name, br.Code, c.wantCode)
		}
		if c.contains != "" && !strings.Contains(br.Brief, c.contains) {
			t.Errorf("%s: brief = %q，未包含 %q", c.name, br.Brief, c.contains)
		}
		if !c.notErased && br.Brief != "" {
			t.Errorf("%s: 空体应得到空 brief，得到 %q", c.name, br.Brief)
		}
	}
}

func TestBriefErrorTruncates(t *testing.T) {
	br := BriefError(strings.Repeat("x", 5000))
	if len([]rune(br.Brief)) > rawMaxRunes+16 {
		t.Fatalf("非 JSON 体未截断: %d runes", len([]rune(br.Brief)))
	}
	if !strings.Contains(br.Brief, "已截断") {
		t.Fatalf("截断未标注: %q", br.Brief)
	}
}
