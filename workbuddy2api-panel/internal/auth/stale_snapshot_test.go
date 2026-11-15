package auth

// stale_snapshot_test.go A4 / W2-C2-17 / 19~27 的回归用例。
//
// 核心场景（审查 A4）：双实例共享 ./auths —— 容器与本机同时跑、滚动发布的两个副本。
// A 实例刚把 refresh token 轮换并落盘，B 实例手里还是轮换前的快照；B 若整份写回，
// A 的新 RT 就被覆盖成**已被上游消费**的旧 RT → 账号掉线且无任何告警。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAuthFile 写一个 auth 文件并返回路径。
func writeAuthFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readAuthFile 读回文件内容（失败即 Fatal）。
func readAuthFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSaveAtomicRefusesStaleSnapshot 旧快照不得覆盖磁盘上更新的 refresh token（审查 A4）。
//
// 构造：写一份 auth → 解析出「旧快照」→ 模拟另一实例刚轮换（直接改磁盘的 RT 与
// ExpiresAt）→ 用旧快照保存 → 必须返回 ErrStaleSnapshot，且磁盘上的新 RT 原封不动。
func TestSaveAtomicRefusesStaleSnapshot(t *testing.T) {
	dir := t.TempDir()
	oldBody := `{"auth":{"accessToken":"at-old","refreshToken":"rt-old","expiresAt":1000},"account":{"uid":"u1"}}`
	path := writeAuthFile(t, dir, "workbuddy-u1.json", oldBody)

	raw := []byte(oldBody)
	stale, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stale.FilePath = path
	stale.noteDiskState(raw) // 这份快照「来自」上面这版字节

	// 另一个实例轮换了 RT 并落盘（新 RT 的到期时刻必然更晚）。
	rotated := `{"auth":{"accessToken":"at-new","refreshToken":"rt-new","expiresAt":9000},"account":{"uid":"u1"}}`
	if err := os.WriteFile(path, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}

	err = stale.SaveAtomic()
	if !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("旧快照保存应返回 ErrStaleSnapshot，实际 err=%v", err)
	}
	if got := readAuthFile(t, path); !strings.Contains(got, "rt-new") || strings.Contains(got, "rt-old") {
		t.Fatalf("磁盘上的新 refresh token 被旧快照覆盖了：%s", got)
	}
}

// TestSaveAtomicSameVersionRewriteStillWorks 同一版本的重写不能被误判成陈旧（审查 A4）。
//
// 误判的代价同样是账号掉线（刷新后的落盘被打断），所以这条负向用例必须存在：
// 解析 → 只改昵称 → 保存，磁盘 RT 没变，必须成功。
func TestSaveAtomicSameVersionRewriteStillWorks(t *testing.T) {
	dir := t.TempDir()
	body := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1000},"account":{"uid":"u1"}}`
	path := writeAuthFile(t, dir, "workbuddy-u1.json", body)

	a, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.FilePath = path
	a.noteDiskState([]byte(body))
	a.Nickname = "改名后"
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("同版本重写应成功，实际 err=%v", err)
	}
	if got := readAuthFile(t, path); !strings.Contains(got, "改名后") {
		t.Fatalf("昵称没写进去：%s", got)
	}
}

// TestRealmMigrationRefusesWhenDiskRotated realm 迁移必须做 CAS（审查 A4 / W2-C2-18）。
//
// 迁移是「解析 → 写回」之间窗口最长的一条路径（LoadDirReport 里整目录扫完才写），
// 也是原报告点名的现场：磁盘一旦被别的实例换过版本，迁移必须放弃而不是整份覆盖。
func TestRealmMigrationRefusesWhenDiskRotated(t *testing.T) {
	dir := t.TempDir()
	oldBody := `{"auth":{"accessToken":"at","refreshToken":"rt-old","expiresAt":1000,"domain":"www.workbuddy.ai"},"account":{"uid":"u1"}}`
	path := writeAuthFile(t, dir, "workbuddy-u1.json", oldBody)

	raw := []byte(oldBody)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a.FilePath = path
	a.noteDiskState(raw)
	if changed, _ := a.BackfillRealm(); !changed {
		t.Fatal("空 realm 应发生变更")
	}

	rotated := `{"auth":{"accessToken":"at-new","refreshToken":"rt-new","expiresAt":9000,"domain":"www.workbuddy.ai"},"account":{"uid":"u1"}}`
	if err := os.WriteFile(path, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.saveRealmMigration(); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("磁盘被改过时迁移应返回 ErrStaleSnapshot，实际 %v", err)
	}
	if got := readAuthFile(t, path); got != rotated {
		t.Fatalf("迁移覆盖了别的实例写入的内容：\n got %s\nwant %s", got, rotated)
	}
}

// TestRealmMigrationOnlyTouchesRealmKey 迁移只改 realm 单键，凭证与未知键原样保留（审查 A4）。
func TestRealmMigrationOnlyTouchesRealmKey(t *testing.T) {
	dir := t.TempDir()
	body := `{"auth":{"accessToken":"at-keep","refreshToken":"rt-keep","expiresAt":1800000000,` +
		`"domain":"www.workbuddy.ai","custom":"keep-me"},"account":{"uid":"u-mig"},"topUnknown":{"a":1}}`
	writeAuthFile(t, dir, "workbuddy-u-mig.json", body)

	if _, err := LoadDir(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(readAuthFile(t, filepath.Join(dir, "workbuddy-u-mig.json"))))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("迁移后的文件不是合法 JSON: %v", err)
	}
	authObj, _ := doc["auth"].(map[string]any)
	if authObj["realm"] != "global" {
		t.Fatalf("realm 没补上：%v", authObj["realm"])
	}
	if authObj["accessToken"] != "at-keep" || authObj["refreshToken"] != "rt-keep" {
		t.Fatalf("迁移改动了凭证：%v", authObj)
	}
	if authObj["expiresAt"] != json.Number("1800000000") {
		t.Fatalf("expiresAt 被改写（数字精度或单位变了）：%v", authObj["expiresAt"])
	}
	if authObj["custom"] != "keep-me" || doc["topUnknown"] == nil {
		t.Fatalf("未知键丢失：auth=%v top=%v", authObj, doc["topUnknown"])
	}
}

// TestAdoptNewerCarriesEncryptedSource 采纳加密来源时必须一并搬 wb* 字段（审查 W2-C2-17）。
//
// 不搬的后果：池里对象来自明文文件（wbSealed=nil），采纳后下一次落盘走明文分支，
// 把桌面端的 $wbEncrypted 文件原地改写成明文 —— 加密保护静默降级。
func TestAdoptNewerCarriesEncryptedSource(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	src, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析加密向量失败: %v", err)
	}
	mem := &Auth{AccessToken: "at-plain-mem", RefreshToken: "rt-plain-mem", ExpiresAt: 1000, UID: "uid-wb-1"}
	if !mem.AdoptNewer(src) {
		t.Fatal("磁盘上更新（到期更晚）的加密凭证应被采纳")
	}
	if mem.wbSealed == nil || mem.wbDoc == nil {
		t.Fatalf("采纳后没有搬 wbSealed/wbDoc：sealed=%v doc=%v", mem.wbSealed != nil, mem.wbDoc != nil)
	}

	path := filepath.Join(t.TempDir(), "auth.info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	mem.FilePath = path
	mem.AccessToken = "at-refreshed-after-adopt"
	if err := mem.SaveAtomic(); err != nil {
		t.Fatalf("采纳后落盘失败: %v", err)
	}
	saved := readAuthFile(t, path)
	if strings.Contains(saved, "at-refreshed-after-adopt") {
		t.Fatalf("加密文件被写成明文：%s", saved)
	}
	again, err := Parse([]byte(saved))
	if err != nil || again.AccessToken != "at-refreshed-after-adopt" {
		t.Fatalf("写回后读不回来: %v %+v", err, again)
	}
}

// TestSaveAtomicRefusesPlaintextOverEncrypted 明文快照不得覆盖加密文件（审查 W2-C2-17）。
func TestSaveAtomicRefusesPlaintextOverEncrypted(t *testing.T) {
	raw, err := os.ReadFile(wbVectorPath)
	if err != nil {
		t.Fatalf("读向量失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "auth.info")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Auth{AccessToken: "at-plain", RefreshToken: "rt-plain", ExpiresAt: 1800000000,
		UID: "uid-wb-1", FilePath: path} // 明文来源：wbSealed 为 nil
	err = a.SaveAtomic()
	if !errors.Is(err, ErrEncryptedDowngrade) {
		t.Fatalf("明文覆盖加密文件应返回 ErrEncryptedDowngrade，实际 %v", err)
	}
	if got := readAuthFile(t, path); got != string(raw) {
		t.Fatalf("加密文件被改动：%s", got)
	}
}

// TestOverwriteInPlaceFixesMode bind-mount 原地覆写路径要顺手收敛权限位（审查 W2-C2-22）。
//
// mode 参数对已存在文件无效，旧版本/外部工具建的 0644 auth 文件会长期全局可读，
// 而正常 rename 路径是 0600 —— 两条路径权限不一致。
func TestOverwriteInPlaceFixesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workbuddy-u1.json")
	if err := os.WriteFile(path, []byte(`{"old":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := overwriteInPlace(path, []byte(`{"new":2}`), 0o600); err != nil {
		t.Fatalf("原地覆写失败: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("覆写后权限 %o want 600（明文凭据不该全局可读）", perm)
	}
	if got := readAuthFile(t, path); got != `{"new":2}` {
		t.Fatalf("覆写内容不对: %s", got)
	}
}

// TestSweepStaleTempsRemovesOldOnly 只清扫超龄临时文件，不动别人正在写的那份（审查 W2-C2-23）。
func TestSweepStaleTempsRemovesOldOnly(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "workbuddy-a.json",
		`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"realm":"cn"},"account":{"uid":"a"}}`)
	oldTmp := writeAuthFile(t, dir, "workbuddy-a.json.tmp.999.1", `{"leak":"token"}`)
	freshTmp := writeAuthFile(t, dir, "workbuddy-a.json.tmp.999.2", `{"leak":"token"}`)
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldTmp, past, past); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadDirReport(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Fatalf("超龄临时文件（含明文凭据）应被清理，err=%v", err)
	}
	if _, err := os.Stat(freshTmp); err != nil {
		t.Fatalf("新鲜的临时文件可能是别的实例正在写，不该删: %v", err)
	}
}

// TestExpiresAtThresholdSharedByBothPaths 秒/毫秒阈值两条路径必须一致（审查 W2-C2-26）。
//
// 落在 [1e11, 1e12) 的值原来被 normalizeExpiresAt 当毫秒（÷1000）、又被
// wbExpiresAtMillis 当秒（回写不 ×1000）→ 凭证被写回成 1/1000，立刻过期。
func TestExpiresAtThresholdSharedByBothPaths(t *testing.T) {
	const v = int64(500_000_000_000) // [1e11, 1e12)：必须是毫秒
	doc := map[string]any{"auth": map[string]any{"expiresAt": json.Number("500000000000")}}
	millis, ok := wbExpiresAtMillis(doc)
	if !ok || !millis {
		t.Fatalf("wbExpiresAtMillis(%d) = (%v,%v) want (true,true)", v, millis, ok)
	}
	sec := normalizeExpiresAt(v)
	if sec != v/1000 {
		t.Fatalf("normalizeExpiresAt(%d) = %d want %d", v, sec, v/1000)
	}
	// 回写按同一单位 → 与原文一致（不再被缩小 1000 倍）。
	s := authFileSnapshot{expiresAt: sec, expiresAtMillis: millis, wbDoc: map[string]any{}}
	if got := wbOverlay(s)["expiresAt"]; got != v {
		t.Fatalf("回写 expiresAt = %v want %d（单位来回不一致）", got, v)
	}
}

// TestSealKeepsOriginalSuite 回写要按原信封的 suite 封装（审查 W2-C2-33）。
//
// 硬编码 suite=1 时，桌面端某字段若是 suite=2，我们一次刷新就把它写成桌面端解不开的
// 形态（用户被迫重新登录，看起来像「插件坏了」）。
func TestSealKeepsOriginalSuite(t *testing.T) {
	env, err := sealWBValueSuite("at-suite2", 2)
	if err != nil {
		t.Fatalf("造 suite=2 信封失败: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"auth":    map[string]any{"accessToken": env, "refreshToken": "rt-plain", "expiresAt": 1800000000},
		"account": map[string]any{"uid": "u-suite"},
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-u-suite.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(body)
	if err != nil || a.AccessToken != "at-suite2" {
		t.Fatalf("解析 suite=2 文件失败: %v %+v", err, a)
	}
	a.FilePath = path
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	dec := json.NewDecoder(strings.NewReader(readAuthFile(t, path)))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("写回不是合法 JSON: %v", err)
	}
	authObj, _ := doc["auth"].(map[string]any)
	envObj, _ := authObj["accessToken"].(map[string]any)
	envB64, _ := envObj["envelope"].(string)
	rawEnv, err := base64.StdEncoding.DecodeString(envB64)
	if err != nil {
		t.Fatalf("信封不是合法 base64: %v", err)
	}
	var meta struct {
		Suite uint32 `json:"suite"`
	}
	if err := json.Unmarshal(rawEnv, &meta); err != nil {
		t.Fatalf("信封不是合法 JSON: %v", err)
	}
	if meta.Suite != 2 {
		t.Fatalf("回写 suite = %d want 2（suite 变了桌面端解不开）", meta.Suite)
	}
	// suite 必须进 AAD，否则 GCM 校验会失败 —— 这里顺带确认能解回来。
	again, err := Parse([]byte(readAuthFile(t, path)))
	if err != nil || again.AccessToken != "at-suite2" {
		t.Fatalf("suite=2 字段写回后读不回来: %v %+v", err, again)
	}
}

// TestIsWBEnvelopeStrict 信封判定要严格（审查 W2-C2-34）。
//
// 宽判定（只判键是否存在）会让 {"$wbEncrypted":0}、空 envelope 这类形状被当成
// 「已封装」：sealWalk 直接跳过、sealForSave 的断言被绕过 → 凭据既没封装也没被拒绝。
func TestIsWBEnvelopeStrict(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"合法信封", map[string]any{"$wbEncrypted": 1, "envelope": "AAAA"}, true},
		{"json.Number 1", map[string]any{"$wbEncrypted": json.Number("1"), "envelope": "AAAA"}, true},
		{"标记为 0", map[string]any{"$wbEncrypted": 0, "envelope": "AAAA"}, false},
		{"标记为 2", map[string]any{"$wbEncrypted": 2, "envelope": "AAAA"}, false},
		{"缺 envelope", map[string]any{"$wbEncrypted": 1}, false},
		{"空 envelope", map[string]any{"$wbEncrypted": 1, "envelope": ""}, false},
		{"envelope 不是字符串", map[string]any{"$wbEncrypted": 1, "envelope": 42}, false},
		{"普通 map", map[string]any{"a": 1}, false},
		{"标量", "x", false},
	}
	for _, c := range cases {
		if got := isWBEnvelope(c.v); got != c.want {
			t.Errorf("%s: isWBEnvelope=%v want %v", c.name, got, c.want)
		}
	}
}

// TestAtRestKeyOverride at-rest 密钥可由环境变量覆盖（审查 W2-C2-31）。
func TestAtRestKeyOverride(t *testing.T) {
	t.Setenv("WB_AT_REST_KEY", "custom-secret-for-this-deploy")
	if got := resolveAtRestSecretKey(); got != "custom-secret-for-this-deploy" {
		t.Fatalf("WB_AT_REST_KEY 未生效，got %q", got)
	}
	t.Setenv("WB_AT_REST_KEY", "")
	t.Setenv("WB_AT_REST_KEY_FILE", "")
	if got := resolveAtRestSecretKey(); got != wbBuiltinAtRestSecretKey {
		t.Fatalf("无覆盖时应回落内置密钥，got %q", got)
	}
}

// TestEnterpriseOverrideValueLocked 人工覆盖值经加锁访问器读取（审查 W2-C2-20）。
func TestEnterpriseOverrideValueLocked(t *testing.T) {
	a := &Auth{AccessToken: "at", UID: "u1", EnterpriseID: "ent-1"}
	if a.IsEnterprise() != true {
		t.Fatal("带 enterpriseId 的号应判企业版（前置条件）")
	}
	// FilePath 为空：SetEnterpriseOverride 会返回错误，但字段必须已经改好（面板据此判权限）。
	_ = a.SetEnterpriseOverride("personal")
	if got := a.EnterpriseOverrideValue(); got != "personal" {
		t.Fatalf("EnterpriseOverrideValue=%q want personal", got)
	}
	if a.IsEnterprise() {
		t.Fatal("人工定性为个人版后 IsEnterprise 必须为 false")
	}
}

// TestTwoInstancesSharedDirNoLostUpdate 双实例共享 auths/ 的端到端形态（审查 A4）。
//
// 现场还原：容器与本机各跑一个网关，共用 ./auths。两个实例都在启动时读到同一份文件；
// A 先完成 refresh 并落盘（上游已轮换 RT），B 的 realm 迁移 / 慢一拍的刷新随后落盘。
// 期望：B 被拒绝、磁盘保持 A 的版本 —— 而不是把 A 的新 RT 覆盖成已作废的旧值。
func TestTwoInstancesSharedDirNoLostUpdate(t *testing.T) {
	dir := t.TempDir()
	body := `{"auth":{"accessToken":"at-0","refreshToken":"rt-0","expiresAt":1000,"domain":"www.workbuddy.ai"},"account":{"uid":"u1"}}`
	path := writeAuthFile(t, dir, "workbuddy-u1.json", body)

	instanceA, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse A: %v", err)
	}
	instanceA.FilePath, instanceA.diskRawOK = path, true
	instanceA.diskRT = "rt-0"
	instanceA.diskRawSHA = sha256.Sum256([]byte(body))

	instanceB, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse B: %v", err)
	}
	instanceB.FilePath = path
	instanceB.noteDiskState([]byte(body))

	// A：刷新成功 → 落盘（RT 轮换 + 到期更晚）。
	instanceA.AccessToken, instanceA.RefreshToken, instanceA.ExpiresAt = "at-1", "rt-1", 5000
	if err := instanceA.SaveAtomic(); err != nil {
		t.Fatalf("A 落盘失败: %v", err)
	}

	// B1：B 只改了个昵称就整份落盘（快照里还是轮换前的 RT 与旧到期时刻）→ 必须被拒。
	// 这是最隐蔽的一条：调用方以为自己只写了 metadata，实际会把 A 的新 RT 一起盖回旧值。
	stale := *instanceB
	stale.Nickname = "改个昵称"
	if err := stale.SaveAtomic(); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("B 的旧快照落盘应被拒（ErrStaleSnapshot），实际 %v", err)
	}

	// B2：realm 迁移（只补标识、不动凭证）→ 同样必须被拒，磁盘不能出现旧 RT。
	if changed, _ := instanceB.BackfillRealm(); !changed {
		t.Fatal("空 realm 应发生变更")
	}
	if err := instanceB.saveRealmMigration(); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatalf("B 的 realm 迁移应被拒，实际 %v", err)
	}

	disk := readAuthFile(t, path)
	if !strings.Contains(disk, `"rt-1"`) || strings.Contains(disk, "rt-0") {
		t.Fatalf("磁盘上的新 RT 被旧快照覆盖：%s", disk)
	}
}

// TestWbOverlayRefusesExpiryOverflow 毫秒回写不得整数溢出（审查 W2-C2-27）。
//
// expiresAt 接近 int64 上限时 ×1000 会溢出成负数 → 落盘一个「已过期」时间 →
// 每次请求都刷新，放大上游调用与风控计数。此时宁可不写该字段（保留磁盘原值）。
func TestWbOverlayRefusesExpiryOverflow(t *testing.T) {
	orig := json.Number("1800000000000")
	s := authFileSnapshot{
		expiresAt: math.MaxInt64/1000 + 1, expiresAtMillis: true,
		wbDoc: map[string]any{"auth": map[string]any{"expiresAt": orig}},
	}
	authObj, _ := wbOverlay(s)["auth"].(map[string]any)
	if got := authObj["expiresAt"]; got != orig {
		t.Fatalf("溢出时应保留磁盘原值 %v，实际 %v", orig, got)
	}
}

// TestKeyIDMismatchDoesNotLeakExpectedID 报错不再打印期望 keyId（审查 W2-C2-35）。
//
// 期望 keyId 是密钥的公开指纹，打出来等于给离线暴力猜密钥的人一个免费校验器。
func TestKeyIDMismatchDoesNotLeakExpectedID(t *testing.T) {
	env, err := sealWBValueSuite("v", 1)
	if err != nil {
		t.Fatalf("造信封失败: %v", err)
	}
	rawEnv, err := base64.StdEncoding.DecodeString(env["envelope"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(rawEnv, &meta); err != nil {
		t.Fatal(err)
	}
	meta["keyId"] = "0000000000000000"
	patched, _ := json.Marshal(meta)
	env["envelope"] = base64.StdEncoding.EncodeToString(patched)
	doc, _ := json.Marshal(map[string]any{
		"auth":    map[string]any{"accessToken": env},
		"account": map[string]any{"uid": "u-leak"},
	})
	_, err = Parse(doc)
	if err == nil || !strings.Contains(err.Error(), "at_rest_key_mismatch") {
		t.Fatalf("keyId 不匹配应报 at_rest_key_mismatch，实际 %v", err)
	}
	want := wbKeyID(sha256.Sum256([]byte(wbAtRestSecretKey)))
	if strings.Contains(err.Error(), want) {
		t.Fatalf("报错泄露了期望 keyId 指纹（%s）: %v", want, err)
	}
}
