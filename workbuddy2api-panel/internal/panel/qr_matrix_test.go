package panel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSQRMatrix 授权链接二维码的编码器必须与规范逐像素一致。
//
// 为什么钉死 sha：面板 CSP 只允许 self（不能引外链 QR 服务），也不引前端依赖，
// 所以 QR 是手写实现 —— 分段、纠错、交织、掩码任一处错都只表现为「扫不出来」，
// 浏览器里没有任何断言能发现。下面的 golden 由 npm `qrcode`（byte 模式 + ECC L +
// mask 0，逐版本用 maskPattern 强制）生成并逐像素 diff=0 交叉验证过。
//
// 版本要求随授权链接变长而涨：加 loginSessionId 后 CN 链接 158 字节（v8），带
// ww_device_nonce 时 207 字节（v9）。v7+ 还多了版本信息块（BCH(6,18)），且
// 对齐图案的中心会落在贯穿线上 —— 这两处都是 v6 之前不存在的代码路径。
func TestAppJSQRMatrix(t *testing.T) {
	nodeBin(t) // 不可执行（Termux 下需 linker64 代跑）时按 helper 的规则 skip
	script := `const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* ── 精简 QR 编码器');
const end = src.indexOf('/* ── 剪贴板复制');
if (start < 0 || end < start) throw new Error('QR block not found');
const ctx = { TextEncoder, Math, Array, Error };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.qrMatrix = qrMatrix;', ctx);
const out = {};
for (const [k, v] of Object.entries(JSON.parse(process.argv[3]))) {
  try {
    const M = ctx.qrMatrix(v);
    out[k] = { n: M.length, rows: M.map(r => r.map(b => b ? '1' : '0').join('')).join('\n') };
  } catch (e) { out[k] = { err: String(e.message) }; }
}
fs.writeFileSync(process.argv[4], JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "qr-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	const st = "12345678-1234-1234-1234-123456789abc"
	const sid = "dcca8b18-4ad8-4a60-aa13-e7592a0e13a4"
	inputs := map[string]string{
		// CN 站真实形态：105 字节 → v5（37×37，单块）
		"cn": "https://copilot.tencent.com/login?platform=CLI&state=" + st + "&version=2.176.0",
		// workbuddy 站：108 字节 → v6（41×41，两块交织）
		"wb": "https://www.workbuddy.ai/login?platform=workbuddy&state=" + st + "&version=2.176.0",
		// 现在面板真实产出的 CN 链接（带 loginSessionId）：158 字节 → v8（49×49）
		"cnSid": "https://www.workbuddy.cn/login?loginSessionId=" + sid + "&platform=workbuddy&state=" + st + "&version=5.9.3",
		// 配了 ww_device_nonce 时：207 字节 → v9（53×53）
		"cnNonce": "https://www.workbuddy.cn/login?loginSessionId=" + sid + "&platform=workbuddy&state=" + st +
			"&version=5.9.3&ww_device_nonce=" + strings.Repeat("n", 32),
		// 容量上界 230 字节；231 必须报错（前端据此退回只显示链接）
		"over": strings.Repeat("a", 231),
	}
	in, _ := json.Marshal(inputs)
	outPath := filepath.Join(t.TempDir(), "out.json")
	if b, err := nodeCmd(f.Name(), "app.js", string(in), outPath).CombinedOutput(); err != nil {
		t.Fatalf("node failed: %v\n%s", err, b)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var got map[string]struct {
		N    int    `json:"n"`
		Rows string `json:"rows"`
		Err  string `json:"err"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad output: %v\n%s", err, raw)
	}

	for _, c := range []struct {
		key     string
		name    string
		modules int
		sha     string
	}{
		{"cn", "CN 站 105 字节 / v5", 37, "2ccaa4c9502d84f197a54ec1d4a174a8312e1db42b3d8a05f306c97477a0a3cd"},
		{"wb", "workbuddy 站 108 字节 / v6 两块交织", 41, "c5818c46fe1cdb1bc829a2249d3fc790f5a0f2c7b141bd196e722bbe5c6ed1ef"},
		{"cnSid", "CN 授权链接 158 字节 / v8（带版本信息块）", 49, "01c41b6708bb8a8f6b309ebef6310ea9719170dee926da1919128bb315f47356"},
		{"cnNonce", "CN 授权链接 207 字节 / v9（带 nonce）", 53, "3af9a315f226ba5c553fc8b1e81bb81a7520ddf400763093178760a64f156d32"},
	} {
		r := got[c.key]
		if r.Err != "" {
			t.Errorf("%s：编码失败 %s", c.name, r.Err)
			continue
		}
		if r.N != c.modules {
			t.Errorf("%s：模块边长 %d，期望 %d（版本选错）", c.name, r.N, c.modules)
			continue
		}
		if h := fmt.Sprintf("%x", sha256.Sum256([]byte(r.Rows))); h != c.sha {
			t.Errorf("%s：矩阵与已验证实现不一致\n got %s\nwant %s", c.name, h, c.sha)
		}
	}
	if r := got["over"]; r.Err == "" {
		t.Errorf("231 字节超出 v9 容量应报错（前端靠它退回链接），实际编码成功 n=%d", r.N)
	}
}
