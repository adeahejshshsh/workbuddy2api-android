#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# build_apk.sh — 一键重新打包 WorkBuddy2API 安卓 App（APK）
#
# 原理：
#   1) 随 APK 分发的服务端二进制必须用【未打 rt0 补丁】的 runtime 编译
#      （APK 内是真 exec，内核栈是标准布局；rt0 补丁是给 linker 代跑模式用的）
#   2) 二进制放进 lib/arm64-v8a/libwb2api.so → 安装器解到 nativeLibraryDir，
#      那是 Android 唯一允许 app 执行的位置
#   3) 资源 aapt2 → Java javac → dex d8(r8.jar) → zip 组装 → apksigner 签名
#
# 用法: bash build_apk.sh            # 输出到工作区 WorkBuddy2API.apk
#       bash build_apk.sh <输出目录>
set -eu
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
GOROOT="$PREFIX/lib/go"
export GOROOT
export PATH="$GOROOT/bin:$PREFIX/bin:$PATH"
export GOPATH="$HOME/go" GOCACHE="$HOME/.cache/go-build" GOMODCACHE="$HOME/go/pkg/mod"
export GOFLAGS="-toolexec=/system/bin/linker64" CGO_ENABLED=0 GOTOOLCHAIN=local GOSUMDB=off
export GOPROXY="file://$PREFIX/lib/workbuddy2api/goproxy,off"

WS="/storage/emulated/0/新建文件夹/workbuddy2api"
SRC="$WS/apk-src"
REPO="$WS/workbuddy2api-panel"
TOOLS="$PREFIX/lib/wbapi-tools"
AJ="$TOOLS/android.jar"; R8="$TOOLS/r8.jar"; SIGNER="$TOOLS/apksigner.jar"
# android.jar 缺失时的降级路径（本机环境曾把整个 wbapi-tools/ 抹掉；SDK 的 stub jar 无法离线重建）：
#   - aapt2 的框架资源表改用系统 /system/framework/framework-res.apk；
#   - Java 源码没动时，直接从上一版 APK 取 classes.dex 复用（并跳过 javac/d8 与看门狗自检）。
#   ⚠ 只要本次改过 apk-src/java/**，必须先补回 android.jar 再构建，否则 dex 会是旧的那份。
AJ_FALLBACK="/system/framework/framework-res.apk"
if [ -f "$AJ" ]; then AAPT_INCLUDE="$AJ"
else
  echo "⚠ 缺 $AJ → aapt2 改用 $AJ_FALLBACK；dex 复用上一版（本次必须无 Java 改动）"
  AAPT_INCLUDE="$AJ_FALLBACK"
fi
OUTDIR="${1:-$WS}"
B="$PREFIX/build/apk"
KS_TARGET="$SRC/wb2api.keystore"; KS_PASS=wb2api2026
mkdir -p "$B/jni/lib/arm64-v8a" "$OUTDIR"

echo "== 1/6 生成 App 用的服务端二进制（未打 rt0 补丁） =="
cp "$GOROOT/src/runtime/asm_arm64.s" "$GOROOT/src/runtime/asm_arm64.s.bak-apkbuild"
cp "$GOROOT/src/runtime/asm_arm64.s.orig-dsh" "$GOROOT/src/runtime/asm_arm64.s"
( cd "$REPO" && go build -buildvcs=false -trimpath -ldflags="-s -w" \
    -o "$B/jni/lib/arm64-v8a/libwb2api.so" ./cmd/server ) || {
  cp "$GOROOT/src/runtime/asm_arm64.s.bak-apkbuild" "$GOROOT/src/runtime/asm_arm64.s"; exit 1; }
# 恢复 DSH 侧（linker 代跑模式）用的补丁版本
if [ -f "$GOROOT/src/runtime/asm_arm64.s.patched-dsh" ]; then
  cp "$GOROOT/src/runtime/asm_arm64.s.patched-dsh" "$GOROOT/src/runtime/asm_arm64.s"
else
  cp "$GOROOT/src/runtime/asm_arm64.s.bak-apkbuild" "$GOROOT/src/runtime/asm_arm64.s"
fi
rm -f "$GOROOT/src/runtime/asm_arm64.s.bak-apkbuild"

echo "== 2/6 资源 =="
cd "$B"
rm -rf res.zip base.apk gen classes dex res      # res：踩过 —— 构建目录里那份陈旧副本
mkdir -p gen classes dex
# 资源目录必须写绝对路径！曾经写成 --dir res（相对），aapt2 就去编译了 $B/res 这份老副本，
# 结果图标/字符串一直是旧的，还一声不响。set -e 也救不了这种"编译了错目录"。
[ -d "$SRC/res" ] || { echo "❌ 找不到资源目录 $SRC/res"; exit 1; }
aapt2 compile --dir "$SRC/res" -o res.zip
aapt2 link -I "$AAPT_INCLUDE" --manifest "$SRC/AndroidManifest.xml" -A "$SRC/assets" --java gen \
  --min-sdk-version 24 --target-sdk-version 34 --version-code 14 --version-name 1.24.1 \
  -o base.apk res.zip

if [ -f "$AJ" ]; then
  echo "== 3/6 javac + 自检 =="
  javac --release 11 -encoding UTF-8 -cp "$AJ" -d classes gen/com/dsh/wb2api/R.java "$SRC"/java/com/dsh/wb2api/*.java
  # 看门狗退避序列自检：宿主 JVM 就能跑，不需要真机（打进 dex 就不必了，单独放 test-out）
  rm -rf test-out && mkdir -p test-out
  javac --release 11 -encoding UTF-8 -cp "$AJ:classes" -d test-out "$SRC"/test/WatchdogSelfCheck.java
  java -cp "$AJ:classes:test-out" WatchdogSelfCheck

  echo "== 4/6 dex (d8) =="
  java -cp "$R8" com.android.tools.r8.D8 --min-api 24 --lib "$AJ" --output dex \
    $(find classes -name '*.class' | tr '\n' ' ')
else
  echo "== 3-4/6 复用上一版 classes.dex（缺 android.jar，跳过 javac/d8/自检） =="
  PREV="$OUTDIR/WorkBuddy2API.apk"
  [ -f "$PREV" ] || { echo "❌ 既无 android.jar 也无上一版 APK，产出不了 dex"; exit 1; }
  python3 - "$PREV" <<'PYEOF'
import sys, zipfile
z = zipfile.ZipFile(sys.argv[1])
open('dex/classes.dex', 'wb').write(z.read('classes.dex'))
print("  复用 %s 的 classes.dex：%d 字节" % (sys.argv[1], z.getinfo('classes.dex').file_size))
PYEOF
fi

echo "== 5/6 组装 APK =="
cp base.apk wb2api-unsigned.apk
( cd dex && zip -q -X ../wb2api-unsigned.apk classes.dex )
( cd jni && zip -q -X ../wb2api-unsigned.apk lib/arm64-v8a/libwb2api.so )

echo "== 6/6 签名 =="
KS="$KS_TARGET"
if [ ! -f "$KS" ]; then
  keytool -genkeypair -keystore "$KS" -storetype PKCS12 -alias wb2api \
    -keyalg RSA -keysize 2048 -validity 10950 -storepass "$KS_PASS" -keypass "$KS_PASS" \
    -dname "CN=WorkBuddy2API, OU=DSH, O=Local, L=Local, ST=Local, C=CN"
fi
java -jar "$SIGNER" sign --ks "$KS" --ks-pass "pass:$KS_PASS" --key-pass "pass:$KS_PASS" \
  --v1-signing-enabled true --v2-signing-enabled true --v3-signing-enabled true \
  --min-sdk-version 24 --out "$OUTDIR/WorkBuddy2API.apk" wb2api-unsigned.apk
java -jar "$SIGNER" verify "$OUTDIR/WorkBuddy2API.apk"

echo
echo "完成: $OUTDIR/WorkBuddy2API.apk"
ls -la "$OUTDIR/WorkBuddy2API.apk"
