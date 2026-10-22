package main

import "testing"

// 版本展示串：VERSION 文件保持纯 x.y.z，构建时间只追加在展示串上。
func TestVersionString(t *testing.T) {
	oldV, oldS := appVersion, buildStamp
	t.Cleanup(func() { appVersion, buildStamp = oldV, oldS })

	appVersion, buildStamp = "1.35.1", ""
	if got := VersionString(); got != "1.35.1" {
		t.Errorf("无构建时间时 = %q want 1.35.1（go run/测试不该带后缀）", got)
	}
	appVersion, buildStamp = "1.35.1", "202610221701"
	if got := VersionString(); got != "1.35.1-202610221701" {
		t.Errorf("有构建时间时 = %q want 1.35.1-202610221701", got)
	}
}
