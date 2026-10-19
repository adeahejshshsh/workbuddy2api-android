#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# patch_source.sh — 对 Go 工具链源码打「本机移植」补丁（幂等，自动备份 .orig-dsh）
#   1) src/runtime/asm_arm64.s → _rt0_arm64 取参修正（argc-1 / argv+16）
#   2) src/net/dnsclient_android.go / dnsclient_unix.go
#      → resolv.conf 路径指向本 App 的 $PREFIX/etc/resolv.conf
#        （/etc 是 /system/etc 只读且没有 resolv.conf，不改则 Go 纯 resolver 必挂）
#
# 用法: bash patch_source.sh            # 打补丁
#       bash patch_source.sh --revert   # 还原
set -eu
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
GOROOT="$PREFIX/lib/go"
RESOLV="$PREFIX/etc/resolv.conf"
MARK="DSH/Android 移植"

if [ "${1:-apply}" = "--revert" ]; then
  echo "== 还原源码补丁 =="
  for f in "$GOROOT/src/runtime/asm_arm64.s" "$GOROOT/src/net/dnsclient_android.go" "$GOROOT/src/net/dnsclient_unix.go"; do
    if [ -f "$f.orig-dsh" ]; then cp "$f.orig-dsh" "$f"; echo "  已还原: $f"; else echo "  无备份: $f"; fi
  done
  echo "提示: 之后跑 rebuild.sh 重编（会重新编译标准库）"
  exit 0
fi

echo "== 打源码补丁 (GOROOT=$GOROOT) =="

# ---------- 1) runtime rt0 ----------
F="$GOROOT/src/runtime/asm_arm64.s"
if grep -qF "$MARK" "$F" 2>/dev/null; then
  echo "  已打过: $F"
elif [ ! -f "$F" ]; then
  echo "  缺文件: $F"
else
  cp "$F" "$F.orig-dsh"
  python3 - "$F" <<'PY'
import sys
f = sys.argv[1]
s = open(f).read()
old = "\tMOVD\t0(RSP), R0\t// argc\n\tADD\t$8, RSP, R1\t// argv\n\tJMP\truntime·rt0_go(SB)"
new = ("\tMOVD\t0(RSP), R0\t// argc\n"
       "\t// DSH/Android 移植: 本机禁止 exec app 私有目录, 二进制一律经\n"
       "\t// /system/bin/linker64 代跑, 栈顶两格是 [发起方 argv0, 程序绝对路径],\n"
       "\t// 与 bionic libc 的 initial_linker_arg_count(=1) 对齐: argc-1, argv+1。\n"
       "\tSUB\t$1, R0\n"
       "\tADD\t$16, RSP, R1\t// argv\n"
       "\tJMP\truntime·rt0_go(SB)")
if old not in s:
    print("  锚点未命中(版本不同?)，请手工核对: " + f); sys.exit(2)
open(f, 'w').write(s.replace(old, new, 1))
print("  已修改: " + f)
PY
  # 退出码必须检查：锚点未命中时 python 以 2 退出，此前无人看，脚本继续往下跑并
  # exit 0 —— 随后 rebuild.sh 编出的二进制 argv 错位（-config 被当普通参数），
  # 启动即退，且没有任何「补丁没打上」的提示。
  [ $? -eq 0 ] || { echo "❌ rt0 补丁失败（Go 版本变了？请手工核对锚点）"; exit 1; }
fi

# ---------- 2) net resolv.conf 路径 ----------
A="$GOROOT/src/net/dnsclient_android.go"
if [ -f "$A" ] && grep -q '/data/data/com.termux/files/usr/etc/resolv.conf' "$A"; then
  cp "$A" "$A.orig-dsh"
  sed -i "s|/data/data/com.termux/files/usr/etc/resolv.conf|$RESOLV|g" "$A"
  echo "  已修改: $A"
else
  echo "  无需处理/已处理: $A"
fi

U="$GOROOT/src/net/dnsclient_unix.go"
if [ -f "$U" ] && grep -q '"/etc/resolv.conf"' "$U"; then
  cp "$U" "$U.orig-dsh"
  sed -i "s|\"/etc/resolv.conf\"|\"$RESOLV\"|g" "$U"
  echo "  已修改: $U"
else
  echo "  无需处理/已处理: $U"
fi

# ---------- 3) net 包：resolv.conf 路径改成「环境变量优先」----------
# ⚠ 这一条是 APK 工况的命门，别再丢：
#   APK 里的 Go 进程跑在 com.dsh.wb2api 下，读不到 DSH 的 $PREFIX；GatewayService
#   通过环境变量 WB2API_RESOLV_CONF 把它自己写的 files/gateway/resolv.conf 传进来。
#   若只做上一步的硬编码路径替换，App 内 DNS 全挂 → 上游全失败 → 全 503 + 连败降权
#   （v1.24.0 真实踩过：两版 .so 对照，旧版含 WB2API_RESOLV_CONF 1 处，新版 0 处）。
python3 - <<'PYEOF'
import os
G = os.path.join(os.environ.get("PREFIX", "/data/data/com.dsharnessmobile.shell/files/usr"), "lib/go/src/net")
HARD = "/data/data/com.dsharnessmobile.shell/files/usr/etc/resolv.conf"
HELPER = """
// wb2apiResolvConfPath 返回本机应读的 resolv.conf 路径（DSH/Android 移植补丁）。
// 顺序：WB2API_RESOLV_CONF 环境变量（APK 注入）→ APK 固定私有路径 → DSH $PREFIX → /etc。
func wb2apiResolvConfPath() string {
	if p := os.Getenv("WB2API_RESOLV_CONF"); p != "" {
		return p
	}
	for _, p := range []string{
		"/data/data/com.dsh.wb2api/files/gateway/resolv.conf",
		"/data/data/com.dsharnessmobile.shell/files/usr/etc/resolv.conf",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/etc/resolv.conf"
}
"""
for f in ("dnsclient_android.go", "dnsclient_unix.go"):
    p = os.path.join(G, f)
    if not os.path.exists(p):
        continue
    s = open(p, encoding="utf-8").read()
    if "func wb2apiResolvConfPath" in s:
        print("  已是环境变量优先:", f)
        continue
    s = s.replace('getSystemDNSConfigNamed("%s")' % HARD, "getSystemDNSConfigNamed(wb2apiResolvConfPath())")
    s = s.replace('dnsReadConfig("%s")' % HARD, "dnsReadConfig(wb2apiResolvConfPath())")
    if '"os"' not in s:
        s = s.replace("import (\n", 'import (\n\t"os"\n', 1)
    s = s.rstrip("\n") + "\n" + HELPER
    open(p, "w", encoding="utf-8").write(s)
    print("  已改为环境变量优先:", f)
PYEOF

echo "== resolv.conf 内容（Go 将读它） =="
cat "$RESOLV" 2>/dev/null || echo "  (缺失！请写入: nameserver 8.8.8.8)"
echo
echo "提示: 已编译好的二进制请用 patches/patch_rt0.py 补；或直接 rebuild.sh 重编。"
