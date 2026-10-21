package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// bug #1：缺 uid 的 auth 文件不能进池。
//
// pool 以 UID 作 map key，空 key 会让 auths/ 下所有缺 uid 的文件互相顶替
// （只有第一个能进，其余被当成重复丢弃），而且面板路由 /accounts/{uid}/...
// 匹配不到空路径段 —— 那个号能发请求却永远复活/禁用/签到/删除点不动。
func TestLoadDirSkipsEmptyUID(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("workbuddy-noUid.json", `{"auth":{"accessToken":"tok-a"}}`)
	write("workbuddy-ok.json", `{"auth":{"accessToken":"tok-b"},"account":{"uid":"u-b"}}`)

	got, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("加载了 %d 个账号，期望 1（缺 uid 的文件必须跳过）", len(got))
	}
	if got[0].UID != "u-b" {
		t.Fatalf("留下的是 %q，期望 u-b", got[0].UID)
	}
}
