package proxypool

import "testing"

// TestRealmDefaultExit realm 级默认出口（issue #39 P3）：账号没有显式绑定时按
// realm 取默认出口；账号级绑定一律优先——「未指定的账号才走 realm 默认」是这条
// 需求的成立前提，优先级反过来会把运维在账号行里点的「直连」静默覆盖掉。
func TestRealmDefaultExit(t *testing.T) {
	m := testManager(t, "")
	a := addProxy(t, m, "http://127.0.0.1:1080")
	b := addProxy(t, m, "http://127.0.0.1:1081")
	m.mu.Lock()
	m.health[a] = health{status: "healthy"}
	m.health[b] = health{status: "healthy"}
	m.mu.Unlock()

	if err := m.BindRealm("global", Binding{Mode: "fixed", ProxyID: b}); err != nil {
		t.Fatalf("BindRealm: %v", err)
	}
	if n, ok, err := m.selectLocked("nobody", "global"); err != nil || !ok || n.ID != b {
		t.Fatalf("global 未绑定账号应走 realm 默认 b，得到 %q ok=%v err=%v", n.ID, ok, err)
	}
	// cn 没设默认 → 仍在可用池里随机（这里是既有行为，只断言走了代理）。
	if _, ok, err := m.selectLocked("nobody", "cn"); err != nil || !ok {
		t.Fatalf("cn 未绑定账号应随机走池，ok=%v err=%v", ok, err)
	}

	// 账号级 direct 必须赢过 realm 默认。
	if err := m.Bind("u1", Binding{Mode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.selectLocked("u1", "global"); err != nil || ok {
		t.Fatalf("账号直连应压过 realm 默认，proxied=%v err=%v", ok, err)
	}

	// realm 默认改成 direct 后该域不再走代理。
	if err := m.BindRealm("global", Binding{Mode: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.selectLocked("nobody", "global"); err != nil || ok {
		t.Fatalf("realm 默认改 direct 后不该再走代理，proxied=%v err=%v", ok, err)
	}

	// random = 删掉该域默认。
	if err := m.BindRealm("global", Binding{Mode: "random"}); err != nil {
		t.Fatal(err)
	}
	if snap := m.Snapshot(); len(snap.Realms) != 0 {
		t.Fatalf("random 应清掉 realm 默认，实际 %v", snap.Realms)
	}
}

// TestRealmDefaultPersists realm 默认要落盘：重启后运维不用重新指一遍。
func TestRealmDefaultPersists(t *testing.T) {
	path := t.TempDir() + "/proxy-pool.json"
	m := testManager(t, path)
	id := addProxy(t, m, "http://127.0.0.1:1080")
	if err := m.BindRealm("cn", Binding{Mode: "fixed", ProxyID: id}); err != nil {
		t.Fatal(err)
	}
	m.Close()

	again, err := New(path, nil, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer again.Close()
	if got := again.Snapshot().Realms["cn"]; got.Mode != "fixed" || got.ProxyID != id {
		t.Fatalf("cn 默认出口没落盘: %+v", got)
	}
}

// TestRealmDefaultBlocksDelete 绑了 realm 默认的代理不许直接删：删掉等于让整域
// 静默掉回随机池，与「不静默改出口」同一原则。
func TestRealmDefaultBlocksDelete(t *testing.T) {
	m := testManager(t, "")
	id := addProxy(t, m, "http://127.0.0.1:1080")
	if err := m.BindRealm("global", Binding{Mode: "fixed", ProxyID: id}); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(id); err != ErrBound {
		t.Fatalf("应回 ErrBound，实际 %v", err)
	}
	if err := m.BindRealm("global", Binding{Mode: "random"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(id); err != nil {
		t.Fatalf("解除后应可删: %v", err)
	}
}

// TestBindRealmRejectsBadInput 只认 cn/global，其它 realm 名直接拒（否则拼错一个
// 字母就得到一条永不生效的配置）。
func TestBindRealmRejectsBadInput(t *testing.T) {
	m := testManager(t, "")
	for _, realm := range []string{"", "CN", "us", "global-x"} {
		if err := m.BindRealm(realm, Binding{Mode: "direct"}); err == nil {
			t.Errorf("realm %q 应被拒绝", realm)
		}
	}
}
