#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# patch_source.sh — 对 Go 工具链源码打「本机移植」补丁（幂等，自动备份 .orig-dsh）
#   1) src/runtime/asm_arm64.s → _rt0_arm64 取参修正（argc-1 / argv+16）
#   2) src/net/dnsclient_android.go / dnsclient_unix.go
#      → resolv.conf 路径指向本 App 的 $PREFIX/etc/resolv.conf
#        （/etc 是 /system/etc 只读且没有 resolv.conf，不改则 Go 纯 resolver 必挂）
#
# 用法: bash patch_source.sh            # 打补丁
#       bash patch_source.sh --revert   # 还原
set -u
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

echo "== resolv.conf 内容（Go 将读它） =="
cat "$RESOLV" 2>/dev/null || echo "  (缺失！请写入: nameserver 8.8.8.8)"
echo
echo "提示: 已编译好的二进制请用 patches/patch_rt0.py 补；或直接 rebuild.sh 重编。"
