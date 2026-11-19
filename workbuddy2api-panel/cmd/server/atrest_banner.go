package main

// at-rest 密钥可见性横幅（W5-03）。
//
// 内置密钥写在公开源码里，等于"全仓共用一把、拿到 auth 文件即可离线解密"。
// 只在真的存在 $wbEncrypted 文件时才打横幅：没有加密文件的部署不受这条影响，
// 天天刷同一行 WARN 会把真正的告警淹掉。

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// maxEncryptedProbeBytes 探测文件是否含 $wbEncrypted 时最多读这么多字节。
// auth 文件正常只有几 KB；上限防止把 GB 级垃圾文件读进内存。
const maxEncryptedProbeBytes = 1 << 20

// encryptedAuthFileCount 统计 dir 下含 "$wbEncrypted" 的普通 auth 文件数。
// 只认 workbuddy*.json 普通文件（跳过 FIFO/符号链接/超大文件），与 LoadDirReport 同口径。
func encryptedAuthFileCount(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if ok, _ := filepath.Match("workbuddy*.json", e.Name()); !ok {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() || info.Size() > maxEncryptedProbeBytes {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		if bytes.Contains(raw, []byte(`"$wbEncrypted"`)) {
			n++
		}
	}
	return n
}

// atRestKeyBanner 返回启动横幅文本；空串表示无需提示。
//
// 触发条件（W5-03）：密钥来自源码内置值 **且** auths/ 里确实存在 $wbEncrypted 文件。
func atRestKeyBanner(info auth.AtRestKeySourceInfo, authDir string) string {
	if !info.Builtin {
		return ""
	}
	n := encryptedAuthFileCount(authDir)
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("!!! 安全横幅（W5-03）：%d 个 auth 文件使用 $wbEncrypted 加密，而 at-rest 密钥来自「%s」"+
		"——该密钥硬编码在公开源码中，全仓/全部署/全账号共用，任何拿到 auth 文件的人都能离线解出 access/refresh token。"+
		"请设置 WB_AT_REST_KEY 或 WB_AT_REST_KEY_FILE 换成部署私有密钥（换后既有加密文件需重新登录导入）。",
		n, info.Detail)
}
