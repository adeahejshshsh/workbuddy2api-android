package proxypool

import (
	"strings"
	"testing"
)

// 探测 URL 不能指向本机/内网/元数据（审查 W2-C6-14）：探测由服务端自己发起，
// 有配置权限者可以让网关去打 169.254.169.254 或内网管理口，并把结果回显到面板。
func TestSettingsRejectsInternalHealthURL(t *testing.T) {
	bad := []string{
		"http://127.0.0.1:8080/",
		"http://localhost:9000/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://[::1]/",
		"http://100.64.0.1/",
		"http://metadata.google.internal/",
		"http://svc.internal/",
	}
	for _, u := range bad {
		s := Settings{URL: u, IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 3, SuccessThreshold: 1}
		if err := s.validate(); err == nil {
			t.Errorf("%s 应被拒绝", u)
		}
	}
	// 公网地址照常通过。
	s := Settings{URL: "https://example.com/healthz", IntervalSeconds: 60, TimeoutSeconds: 5, FailureThreshold: 3, SuccessThreshold: 1}
	if err := s.validate(); err != nil {
		t.Errorf("公网地址不该被拒: %v", err)
	}
}

// 单次导入有字节与条数上限（审查 W2-C6-13）：解析此前在锁内跑，
// 一次超大粘贴就能让 selectLocked（每请求热路径）排队到超时。
func TestImportRejectsOversizedInput(t *testing.T) {
	m := testManager(t, "")
	if _, err := m.Import(strings.Repeat("x", maxImportBytes+1)); err == nil {
		t.Fatal("超过字节上限应被拒")
	}
	var b strings.Builder
	for i := 0; i < maxImportNodes+1; i++ {
		b.WriteString("http://u:p@host")
		b.WriteString(strings.Repeat("a", i%7))
		b.WriteString(".example.com:8080\n")
	}
	if _, err := m.Import(b.String()); err == nil {
		t.Fatal("超过条数上限应被拒")
	}
}

// 删节点时两张只增不删的表要一起清（审查 W2-C6-16）：订阅刷新场景反复导入/删除会泄漏。
func TestDeleteClearsAuxMaps(t *testing.T) {
	m := testManager(t, "")
	res, err := m.Import("http://u:p@1.2.3.4:8080\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Status != "added" {
		t.Fatalf("导入应成功: %+v", res)
	}
	id := m.data.Nodes[0].ID
	m.mu.Lock()
	m.generation[id]++
	m.requested[id] = true
	m.mu.Unlock()
	if err := m.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	m.mu.Lock()
	_, g := m.generation[id]
	_, r := m.requested[id]
	m.mu.Unlock()
	if g || r {
		t.Fatalf("删除后辅助表仍有残留：generation=%v requested=%v", g, r)
	}
}

// Snapshot 遇到无法解析的 URL 不能 panic（审查 W2-C6-12）。
func TestSnapshotToleratesBadURL(t *testing.T) {
	m := testManager(t, "")
	m.mu.Lock()
	m.data.Nodes = append(m.data.Nodes, node{ID: "bad", Name: "bad", URL: "://nonsense", Enabled: true})
	m.mu.Unlock()
	s := m.Snapshot() // 修复前这里 nil 解引用 panic
	if len(s.Proxies) != 1 {
		t.Fatalf("坏 URL 也应出现在快照里（带错误说明），实际 %d 条", len(s.Proxies))
	}
	if !strings.Contains(s.Proxies[0].LastError, "无法解析") {
		t.Fatalf("应带解析失败说明，实际 %q", s.Proxies[0].LastError)
	}
}
