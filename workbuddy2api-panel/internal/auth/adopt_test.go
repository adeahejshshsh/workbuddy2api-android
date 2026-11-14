package auth

import "testing"

// AdoptNewer 只把**更新的**令牌原地拷进来：换指针会让刚刷新的令牌被磁盘旧值盖回去，
// 而扫描早于刷新写回时磁盘那份可能已经是作废的 refresh token。
func TestAdoptNewerOnlyWhenDiskIsNewer(t *testing.T) {
	base := func(at string, exp int64) *Auth {
		return &Auth{AccessToken: at, RefreshToken: "rt-" + at, ExpiresAt: exp, Domain: "www.workbuddy.ai", UID: "u1"}
	}

	// 磁盘同到期、不同令牌 → 不动（典型的「扫描早于刷新写回」快照）。
	mem := base("at-new", 2000)
	if mem.AdoptNewer(base("at-old", 2000)) {
		t.Fatal("同到期时刻的旧令牌不该覆盖内存里的新令牌")
	}
	if mem.AccessToken != "at-new" {
		t.Fatalf("令牌被覆盖成 %q", mem.AccessToken)
	}

	// 磁盘更晚 → 采纳（典型是外部重新登录）。
	mem = base("at-old", 1000)
	if !mem.AdoptNewer(base("at-new", 2000)) {
		t.Fatal("磁盘上更新（过期更晚）的令牌应被采纳")
	}
	if mem.AccessToken != "at-new" || mem.ExpiresAt != 2000 || mem.RefreshToken != "rt-at-new" {
		t.Fatalf("采纳后字段不对：%+v", mem)
	}

	// 静态 API Key（两边 expiresAt 都是 0）：以令牌不同为准。
	mem = base("key-1", 0)
	if !mem.AdoptNewer(base("key-2", 0)) {
		t.Fatal("expiresAt 都为 0 的静态 Key 应以令牌不同为准")
	}

	// 空令牌 / 自己 → 不动。
	mem = base("at-x", 1000)
	if mem.AdoptNewer(mem) {
		t.Fatal("自己不该被采纳")
	}
	if mem.AdoptNewer(base("", 9999)) {
		t.Fatal("空令牌不该被采纳")
	}
}
