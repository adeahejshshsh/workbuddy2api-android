package panel

// 导入形态兼容：不同导出工具给的 JSON 结构差异很大，用户不会区分。
// 这里钉住四类常见形态 + 一类必须拒绝的垃圾结构。
//
// 尤其是「信封的值是单个对象」那条：`{"accounts":{"refresh_token":"r"}}` 走到字典
// 分支时，如果拿信封名当 uid，池里会多一个 uid="accounts" 的垃圾号 —— 格式合法、
// 内容全错，排障时最难查。

import (
	"encoding/json"
	"testing"
)

func decodeFirst(t *testing.T, raw string) cockpitAccount {
	t.Helper()
	list, err := parseImportPayload([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v (raw=%s)", err, raw)
	}
	if len(list) != 1 {
		t.Fatalf("条目数=%d want 1 (raw=%s)", len(list), raw)
	}
	return list[0]
}

func TestImportShapesAccepted(t *testing.T) {
	// 1) 顶层数组（批量导出，snake_case）
	acc := decodeFirst(t, `[{"uid":"u1","access_token":"at","refresh_token":"rt","expires_at":1760000000000}]`)
	if acc.UID != "u1" || acc.AccessToken != "at" || acc.RefreshToken != "rt" {
		t.Fatalf("数组形态解析错: %+v", acc)
	}

	// 2) 单账号嵌套驼峰（cockpit 详情导出 {account,auth}）
	acc = decodeFirst(t, `{"account":{"uid":"u2","nickname":"小明","enterpriseId":"e1"},
		"auth":{"accessToken":"at2","refreshToken":"rt2","expiresAt":1760000000,"domain":"https://api.workbuddy.ai"}}`)
	if acc.UID != "u2" || acc.AccessToken != "at2" || acc.ExpiresAt != 1760000000 || acc.EnterpriseID != "e1" {
		t.Fatalf("嵌套驼峰解析错: %+v", acc)
	}

	// 3) 信封值是一串账号（data 键）
	acc = decodeFirst(t, `{"code":0,"data":[{"uid":"u3","access_token":"at3","refresh_token":"rt3"}]}`)
	if acc.UID != "u3" {
		t.Fatalf("信封数组解析错: %+v", acc)
	}

	// 4) 信封值是单个对象（用户把详情又包了一层）
	acc = decodeFirst(t, `{"accounts":{"uid":"u4","access_token":"at4","refresh_token":"rt4"}}`)
	if acc.UID != "u4" {
		t.Fatalf("信封单对象解析错（uid 不能取信封名）: %+v", acc)
	}

	// 5) 字典 {"<uid>": {...}}：键兜底成 uid
	acc = decodeFirst(t, `{"13800000000":{"access_token":"at5","refresh_token":"rt5"}}`)
	if acc.UID != "13800000000" {
		t.Fatalf("字典形态键应兜底成 uid: %+v", acc)
	}
}

// TestImportEnvelopeNameNeverBecomesUID 信封名不是 uid：没有 uid 字段时宁可空着
// （主循环记 skipped），也不能把 "accounts" 当手机号写进池。
func TestImportEnvelopeNameNeverBecomesUID(t *testing.T) {
	list, err := parseImportPayload([]byte(`{"accounts":{"refresh_token":"r","access_token":"a"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("条目数=%d want 1", len(list))
	}
	if list[0].UID != "" {
		t.Fatalf("uid 不能是信封名，got %q", list[0].UID)
	}
}

func TestImportRejectsGarbage(t *testing.T) {
	if _, err := parseImportPayload([]byte(`{"foo":"bar"}`)); err == nil {
		t.Fatal("既不是账号对象也不是字典的结构应报错")
	}
	if _, err := parseImportPayload([]byte(`"just a string"`)); err == nil {
		t.Fatal("字符串应报错")
	}
}

// TestImportDictStableOrder 字典形态按 key 排序输出：导入结果的前端展示与测试比对
// 都需要稳定顺序，map 遍历随机会让「第 N 条失败」的提示每次不同。
func TestImportDictStableOrder(t *testing.T) {
	raw := `{"b":{"access_token":"x","refresh_token":"y"},"a":{"access_token":"x","refresh_token":"y"}}`
	list, err := parseImportPayload([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].UID != "a" || list[1].UID != "b" {
		got, _ := json.Marshal(list)
		t.Fatalf("顺序应稳定为 a,b，got %s", got)
	}
}
