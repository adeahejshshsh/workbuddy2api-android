package panel

// T-10 / 审查 W6-02（P1）：apikey.go 的 addAPIKey / resolveAPIKeyUID / apiKeyID
// 在 460 个 Test 里**零引用**。
//
// 这三个函数是「静态 API Key 账号」的完整入口：校验前缀 → 派生落盘 uid → 探测额度
// → 落盘 → 入池。它们的风险不是「报错」，而是**静默的错值**：
//   - uid 派生不做净化 → 路径穿越（key 里带 ../ 就能把文件写到 auths/ 之外）；
//   - 探测失败仍落盘 → 池里多一个坏号，选号反复命中它，表现为「网关时好时坏」；
//   - 额度探测成功但没写进池 → 面板显示 0 积分，credit_floor 把这个号挡出选号；
//   - 空 token 能加进来 → 等价于把一个「永远 401」的号塞进池子（与 W5-T3-05
//     叠加：鉴权只测了「对的 key 放行」，key 的写入路径又不测）。
//
// 既有 9 个测试文件的 `APIKey:` 命中全是 Config 字段，没有一条打到这些函数。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// enterpriseUsagePath 静态 API Key 的额度口径端点。
//
// upstream.enterpriseResourceCtx 里写死这条路径；apikey.go 的探测走的正是它
// （UsesEnterpriseBilling 对 IsAPIKey() 恒真）。这里抄一份是为了让断言能指出
// 「打错端点」这件事 —— 打个人端点不会报错，只会静默拿到 0/0。
const enterpriseUsagePath = "/v2/billing/meter/get-enterprise-user-usage"

// apiKeyEnv 搭一个可落盘、可探测额度的面板。
//
// probe 收到的是上游请求（billing/meter/get-user-resource），返回 (status, body)。
// AuthDir 用 t.TempDir()：本文件多条用例的核心断言是「文件有没有落盘」，
// 落在真实目录里既不可断言也会污染开发机。
func apiKeyEnv(t *testing.T, probe func(*http.Request) (int, string)) (*Panel, *pool.Pool, string) {
	t.Helper()
	dir := t.TempDir()
	up := &upstream.Client{
		HTTP: &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
			status, body := probe(r)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		})},
		ChatBaseCN: "https://cn.example", WebBaseCN: "https://cn.example", BillingBaseCN: "https://cn.example",
	}
	pl := pool.New("")
	return New(Config{
		Pool: pl, Upstream: up, AuthDir: dir,
		APIKey: "test-key", AdminAPIKey: "test-key", // add_apikey 是敏感接口，必须带管理员 key
	}), pl, dir
}

// addAPIKeyPost 发一次 POST /panel/api/accounts/add_apikey（带管理员 key）。
func addAPIKeyPost(t *testing.T, p *Panel, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/add_apikey", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// authDirFiles 列出 auths 目录里的文件名（不存在时返回空）。
func authDirFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读 auths 目录: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestAPIKeyIDStripsPrefixAndDot apiKeyID 只保留 <id> 段且只留安全字符。
//
// 不变量：结果只含 [A-Za-z0-9_-]，且已剥掉 ck_/pt_ 前缀与首个点之后的 secret。
// 这是「拼文件名前必须净化」的唯一一道闸 —— 结果里出现 . 或 / 就是路径穿越。
func TestAPIKeyIDStripsPrefixAndDot(t *testing.T) {
	cases := []struct{ name, key, want string }{
		{"ck_ 前缀 + 点分 secret", "ck_abc.def", "abc"},
		{"pt_ 前缀 + 点分 secret", "pt_xyz.secret", "xyz"},
		{"无点：整段都是 id", "ck_onlyid", "onlyid"},
		{"无前缀：前缀只作裁剪不校验", "plain.id", "plain"},
		{"只有前缀", "ck_", ""},
		{"空串", "", ""},
		{"只有点", "ck_.secret", ""},
		{"非法字符被剔除", "ck_a/b\\c.d", "abc"},
		{"保留连字符与下划线", "ck_a-b_c.d", "a-b_c"},
		{"大写保留", "ck_ABC.DEF", "ABC"},
		{"非 ASCII 被剔除", "ck_中文id.secret", "id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := apiKeyID(c.key); got != c.want {
				t.Fatalf("apiKeyID(%q)=%q want %q", c.key, got, c.want)
			}
		})
	}
}

// TestAPIKeyIDNeverYieldsPathSeparators 恶意 key 派生出的 id 不得带路径成分。
//
// 这条守的是「加一个号 → 全机任意文件可写」：uid 直接拼进
// filepath.Join(AuthDir, "workbuddy-"+uid+".json")，一旦 id 里能留下 "/" 或 ".."，
// 落盘路径就跳出 AuthDir。
func TestAPIKeyIDNeverYieldsPathSeparators(t *testing.T) {
	evil := []string{
		"ck_../../etc/passwd.x",
		"ck_..\\..\\windows\\system32.x",
		"ck_/etc/shadow.x",
		"ck_a/../../b.x",
		"pt_....//....//x.y",
	}
	for _, key := range evil {
		id := apiKeyID(key)
		if strings.ContainsAny(id, "/\\.:") {
			t.Fatalf("apiKeyID(%q)=%q 仍含路径成分", key, id)
		}
		// 端到端再确认一次：派生出的 uid 拼出的文件名必须还在 AuthDir 内。
		uid, err := resolveAPIKeyUID("", key)
		if err != nil {
			continue // 派生失败（空 id）也是安全的
		}
		if strings.ContainsAny(uid, "/\\.:") {
			t.Fatalf("resolveAPIKeyUID(%q)=%q 仍含路径成分", key, uid)
		}
		full := filepath.Join("/auths", "workbuddy-"+uid+".json")
		if filepath.Dir(full) != "/auths" {
			t.Fatalf("派生 uid 把落盘路径带出了 AuthDir: %q", full)
		}
	}
}

// TestResolveAPIKeyUIDExplicitAndDerived 显式 uid 走白名单校验，缺省时派生 apikey-<id>。
//
// 不变量：
//   - 显式 uid 必须过 validImportUID（字母/数字/下划线/连字符，≤64）——与导入同一
//     判据，否则「导入拒绝的 uid」能从这里绕过；
//   - 派生 uid 超长必须截断到 64（文件名上限的兜底），且保留 apikey- 前缀；
//   - 无法派生 id 时必须报错而不是落一个空 uid 的文件。
func TestResolveAPIKeyUIDExplicitAndDerived(t *testing.T) {
	// 显式 uid 合法：原样采用（含首尾空白裁剪）。
	if got, err := resolveAPIKeyUID("  my-uid_1  ", "ck_abc.def"); err != nil || got != "my-uid_1" {
		t.Fatalf("显式 uid: got=%q err=%v want my-uid_1", got, err)
	}
	// 显式 uid 非法：路径穿越、空格、超长都必须拒绝。
	bad := []string{"../evil", "a/b", "a\\b", "a b", "uid.with.dot", strings.Repeat("x", apiKeyUIDMaxLen+1)}
	for _, uid := range bad {
		if got, err := resolveAPIKeyUID(uid, "ck_abc.def"); err == nil {
			t.Fatalf("显式 uid %q 应被拒绝，实际得到 %q", uid, got)
		}
	}
	// 缺省派生：apikey-<id>。
	if got, err := resolveAPIKeyUID("", "ck_abc.def"); err != nil || got != "apikey-abc" {
		t.Fatalf("派生 uid: got=%q err=%v want apikey-abc", got, err)
	}
	// 空 id：必须报错（否则会落一个 workbuddy-apikey-.json）。
	if got, err := resolveAPIKeyUID("", "ck_.secret"); err == nil {
		t.Fatalf("空 id 应报错，实际得到 %q", got)
	}
	// 超长 id：截断到 64，且前缀保留（前缀丢了就与显式 uid 撞名）。
	long := "ck_" + strings.Repeat("z", 200) + ".secret"
	got, err := resolveAPIKeyUID("", long)
	if err != nil {
		t.Fatalf("超长 id 应截断而非报错: %v", err)
	}
	if len(got) != apiKeyUIDMaxLen {
		t.Fatalf("超长 uid 长度=%d want %d", len(got), apiKeyUIDMaxLen)
	}
	if !strings.HasPrefix(got, "apikey-") {
		t.Fatalf("截断后丢了 apikey- 前缀: %q", got)
	}
}

// TestAddAPIKeyRejectsInvalidInput 入参校验必须挡在落盘之前。
//
// 每条 case 都断言「400 + auths 目录里一个文件都没有」：只断言状态码不够 ——
// 「先落盘再校验」的实现同样会回 400，但坏号已经进了池子。
func TestAddAPIKeyRejectsInvalidInput(t *testing.T) {
	probeCalls := 0
	p, _, dir := apiKeyEnv(t, func(*http.Request) (int, string) {
		probeCalls++
		return http.StatusOK, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`
	})

	cases := []struct{ name, body string }{
		{"畸形 JSON", `{"access_token":`},
		{"缺 access_token", `{"uid":"u1"}`},
		{"空 access_token", `{"access_token":"   "}`},
		{"非 ck_/pt_ 前缀", `{"access_token":"Bearer at_123"}`},
		{"OAuth 形态的 access token", `{"access_token":"at-abcdef"}`},
		{"显式 uid 含路径穿越", `{"access_token":"ck_abc.def","uid":"../evil"}`},
		{"显式 uid 超长", `{"access_token":"ck_abc.def","uid":"` + strings.Repeat("x", 100) + `"}`},
		{"key 无法派生 id 且未给 uid", `{"access_token":"ck_.secret"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := addAPIKeyPost(t, p, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400（body=%s）", rec.Code, rec.Body)
			}
			if files := authDirFiles(t, dir); len(files) != 0 {
				t.Fatalf("校验失败却落了盘：%v", files)
			}
		})
	}
	// 全部挡在探测之前：无效入参不该花一次真实计费的上游请求。
	if probeCalls != 0 {
		t.Fatalf("无效入参仍打了 %d 次上游额度探测", probeCalls)
	}
}

// TestAddAPIKeyProbeFailureWritesNothing 探测失败必须不落盘（函数注释的核心承诺）。
//
// 为什么重要：静态 Key 没有任何本地可验证的签名字段，探测是唯一验证。落一个坏号
// 进池子的表现是「网关时好时坏」——选号反复命中它、每次失败都要冷却换号，
// 而面板上它看起来是个正常账号。
func TestAddAPIKeyProbeFailureWritesNothing(t *testing.T) {
	p, pl, dir := apiKeyEnv(t, func(*http.Request) (int, string) {
		// 400 是确定性拒绝（非瞬时错误，不会触发计费重试的 2s/4s 退避）。
		return http.StatusBadRequest, `{"code":40100,"msg":"invalid api key"}`
	})
	rec := addAPIKeyPost(t, p, `{"access_token":"ck_badkey.secret","nickname":"坏号"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（body=%s）", rec.Code, rec.Body)
	}
	if files := authDirFiles(t, dir); len(files) != 0 {
		t.Fatalf("探测失败仍落了盘：%v", files)
	}
	if _, ok := pl.Status("apikey-badkey"); ok {
		t.Fatal("探测失败仍把号加进了池子")
	}
	if a := pl.AuthByUID("apikey-badkey"); a != nil {
		t.Fatal("探测失败仍把凭证放进了池子")
	}
}

// TestAddAPIKeySavesAndPoolsWithCredits 成功路径：落盘 + 入池 + 额度写入 + 响应契约。
//
// 不变量：
//   - 探测必须打**企业口径**端点（静态 Key 在个人端点恒返回空 Accounts，走错端点
//     的表现是「每个新加的 key 都显示 0 积分」，见 apikey.go 的注释）；
//   - 落盘文件名是 workbuddy-<派生 uid>.json（与 OAuth 账号同形，热重载才认得）；
//   - 落盘内容能被 auth.LoadDir 读回，且是静态 Key 形态（无 refreshToken/expiresAt）；
//   - 探测到的额度必须写进池（否则 credit_floor 把这个好号挡出去）；
//   - 响应体回 uid/nickname/api_key:true —— 面板靠它显示「已添加」。
func TestAddAPIKeySavesAndPoolsWithCredits(t *testing.T) {
	var probed []string
	p, pl, dir := apiKeyEnv(t, func(r *http.Request) (int, string) {
		probed = append(probed, r.URL.Path)
		if !strings.HasSuffix(r.URL.Path, enterpriseUsagePath) {
			return http.StatusOK, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`
		}
		return http.StatusOK, `{"code":0,"data":{"credit":500,"limitNum":1000}}`
	})
	rec := addAPIKeyPost(t, p, `{"access_token":"ck_goodkey.secret","nickname":" 静态号 "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（body=%s）", rec.Code, rec.Body)
	}
	var got struct {
		OK       bool   `json:"ok"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		APIKey   bool   `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应: %v（%s）", err, rec.Body)
	}
	if !got.OK || got.UID != "apikey-goodkey" || !got.APIKey {
		t.Fatalf("响应契约错: %+v", got)
	}
	if got.Nickname != "静态号" {
		t.Fatalf("nickname=%q want 静态号（应裁掉首尾空白）", got.Nickname)
	}

	// 落盘：文件名与内容都要能被热重载读回。
	want := filepath.Join(dir, "workbuddy-apikey-goodkey.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("没有落盘到 %s: %v（现有文件 %v）", want, err, authDirFiles(t, dir))
	}
	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("LoadDir 读到 %d 个账号 want 1", len(auths))
	}
	a := auths[0]
	if a.UID != "apikey-goodkey" || a.AccessToken != "ck_goodkey.secret" {
		t.Fatalf("落盘字段错: uid=%q at=%q", a.UID, a.AccessToken)
	}
	if a.RefreshToken != "" || a.ExpiresAt != 0 {
		t.Fatalf("静态 Key 不该有 refreshToken/expiresAt: rt=%q exp=%d", a.RefreshToken, a.ExpiresAt)
	}
	if !a.IsAPIKey() {
		t.Fatal("读回的账号不被识别为 API Key（realm/前缀判据漂了）")
	}

	// 入池 + 额度：探测到的 500/1000 必须落到池里。
	st, ok := pl.Status("apikey-goodkey")
	if !ok {
		t.Fatal("账号没有入池")
	}
	if st.Credits != 500 || st.CreditsTotal != 1000 {
		t.Fatalf("池内额度 credits=%d total=%d want 500/1000（探测路径 %v）", st.Credits, st.CreditsTotal, probed)
	}
	if len(probed) != 1 || !strings.HasSuffix(probed[0], enterpriseUsagePath) {
		t.Fatalf("额度探测路径=%v want 恰好一次 %s（静态 Key 走个人端点会恒返回空 Accounts）",
			probed, enterpriseUsagePath)
	}
}

// TestAddAPIKeyProbeHitsEnterpriseEndpointOnly 静态 Key 的额度探测必须走企业口径端点。
//
// 依据 apikey.go 的注释：静态 Key 没有任何本地可验证的签名字段，唯一可靠的验证就是
// 拿它打一次真实端点；而个人端点（get-user-resource）对它**恒返回空 Accounts**
// （code 0 + Accounts null）—— 走错端点的表现是「key 加进去了、额度显示 0/0」，
// 于是 credit_floor 立刻把这个好号挡出选号，而面板上看起来一切正常。
//
// 断言方式：把两条路径都挂上，个人端点回空 Accounts、企业端点回 500/1000，
// 然后检查**实际被调用的**是哪一个（顺带证明没有「先打个人再 fallback」的浪费）。
func TestAddAPIKeyProbeHitsEnterpriseEndpointOnly(t *testing.T) {
	var probed []string
	p, pl, _ := apiKeyEnv(t, func(r *http.Request) (int, string) {
		probed = append(probed, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			return http.StatusOK, `{"code":0,"data":{"Response":{"Data":{"Accounts":null}}}}`
		}
		return http.StatusOK, `{"code":0,"data":{"credit":0,"limitNum":0}}`
	})
	rec := addAPIKeyPost(t, p, `{"access_token":"ck_probeonly.secret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（body=%s）", rec.Code, rec.Body)
	}
	for _, path := range probed {
		if strings.HasSuffix(path, "/get-user-resource") {
			t.Fatalf("探测打了个人口径端点 %s：静态 Key 在那里恒返回空 Accounts", path)
		}
	}
	if len(probed) == 0 {
		t.Fatal("一次额度探测都没发生")
	}
	if _, ok := pl.Status("apikey-probeonly"); !ok {
		t.Fatal("账号没有入池")
	}
}
