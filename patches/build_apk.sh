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

# 版本号集中在这里：aapt2 路径写进清单；无 aapt2 的降级路径就地把旧清单里的版本串
# 等长改写（AXML 字符串池是 UTF-16LE，等长替换不破坏结构）。
VERCODE=24
VERNAME="1.26.0"

WS="/storage/emulated/0/新建文件夹/workbuddy2api"
SRC="$WS/apk-src"
REPO="$WS/workbuddy2api-panel"
TOOLS="$PREFIX/lib/wbapi-tools"
AJ="$TOOLS/android.jar"
# r8.jar / apksigner.jar 曾在环境重置时被抹掉，现在改用 Termux 包自带的那份
# （$PREFIX/share/java/）。两处都探测：优先 wbapi-tools 的私有副本。
R8="$TOOLS/r8.jar";              [ -f "$R8" ]     || R8="$PREFIX/share/java/d8.jar"
SIGNER="$TOOLS/apksigner.jar";   [ -f "$SIGNER" ] || SIGNER="$PREFIX/share/java/apksigner.jar"
[ -f "$R8" ]     || { echo "❌ 找不到 d8/r8 jar（$TOOLS/r8.jar 与 $PREFIX/share/java/d8.jar 都不存在）"; exit 1; }
[ -f "$SIGNER" ] || { echo "❌ 找不到 apksigner jar"; exit 1; }
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
# GOROOT 源码会被临时换成「未打 rt0 补丁」的版本，还原必须做成退出钩子而不是顺序
# 语句：构建中途被 Ctrl-C / 休眠 / OOM 打断，或 .orig-dsh 缺失导致 set -e 直接退出时，
# 顺序还原永远走不到，GOROOT 就永久停在未打补丁状态 —— 之后所有 run.sh/rebuild.sh
# 都静默产出 argv 错位的二进制（启动即退，且毫无提示）。
ASM="$GOROOT/src/runtime/asm_arm64.s"
ASM_BAK="$GOROOT/src/runtime/asm_arm64.s.bak-apkbuild"
restore_asm() {
  if [ -f "$ASM_BAK" ]; then
    if [ -f "$GOROOT/src/runtime/asm_arm64.s.patched-dsh" ]; then
      cp "$GOROOT/src/runtime/asm_arm64.s.patched-dsh" "$ASM"
    else
      cp "$ASM_BAK" "$ASM"
    fi
    rm -f "$ASM_BAK"
  fi
}
trap 'restore_asm' EXIT INT TERM HUP
[ -f "$GOROOT/src/runtime/asm_arm64.s.orig-dsh" ] || {
  echo "❌ 缺 $GOROOT/src/runtime/asm_arm64.s.orig-dsh（先跑 patch_source.sh）"; exit 1; }
cp "$ASM" "$ASM_BAK"
cp "$GOROOT/src/runtime/asm_arm64.s.orig-dsh" "$ASM"
( cd "$REPO" && go build -buildvcs=false -trimpath -ldflags="-s -w" \
    -o "$B/jni/lib/arm64-v8a/libwb2api.so" ./cmd/server )
restore_asm
trap - EXIT INT TERM HUP

echo "== 2/6 资源 =="
cd "$B"
rm -rf res.zip base.apk gen classes dex res      # res：踩过 —— 构建目录里那份陈旧副本
mkdir -p gen classes dex
# aapt2 可用性探测：本机 aapt2 与仓库最新 abseil/protobuf ABI 不匹配时（缺
# ZeroCopyInputStream::ReadCord 符号）会直接崩，那种情况下走降级路径。
if aapt2 version >/dev/null 2>&1; then AAPT_OK=1; else AAPT_OK=0; fi
if [ "$AAPT_OK" = "1" ]; then
  # 资源目录必须写绝对路径！曾经写成 --dir res（相对），aapt2 就去编译了 $B/res 这份老副本，
  # 结果图标/字符串一直是旧的，还一声不响。set -e 也救不了这种"编译了错目录"。
  [ -d "$SRC/res" ] || { echo "❌ 找不到资源目录 $SRC/res"; exit 1; }
  aapt2 compile --dir "$SRC/res" -o res.zip
  aapt2 link -I "$AAPT_INCLUDE" --manifest "$SRC/AndroidManifest.xml" -A "$SRC/assets" --java gen \
    --min-sdk-version 24 --target-sdk-version 34 --version-code $VERCODE --version-name $VERNAME \
    -o base.apk res.zip
else
  echo "⚠ aapt2 不可用 → 复用上一版 APK 的清单与资源（只重编 dex 与 .so）"
  PREV="$OUTDIR/WorkBuddy2API.apk"
  [ -f "$PREV" ] || { echo "❌ 既无可用 aapt2 也无上一版 APK，生成不了清单与资源"; exit 1; }
  python3 - "$PREV" base.apk "$VERNAME" <<'PYAXML'
import sys, zipfile, re
prev, out, newver = sys.argv[1], sys.argv[2], sys.argv[3]
src = zipfile.ZipFile(prev)
man = src.read("AndroidManifest.xml")
# AXML 的字符串池是 UTF-16LE：把 "1.25.0" 这样的版本串按 UTF-16LE 找出来。
pat = re.compile(rb"(?:1\x00\.\x00\d\x00\d\x00\.\x00\d\x00)")
hits = set(pat.findall(man))
assert len(hits) == 1, "版本串定位失败（命中 %d 处）" % len(hits)
old = hits.pop().decode("utf-16-le")
if old != newver:
    assert len(old) == len(newver), "版本串长度必须一致：%s → %s" % (old, newver)
    man = man.replace(old.encode("utf-16-le"), newver.encode("utf-16-le"))
    print("  清单版本串 %s → %s（等长就地改写）" % (old, newver))
else:
    print("  清单版本串已是 %s" % newver)
dst = zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED)
kept = 0
for it in src.infolist():
    n = it.filename
    if n == "AndroidManifest.xml":
        dst.writestr(n, man); kept += 1; continue
    # 签名块与 dex/lib 一律不复用（签名块由 apksigner 重写，dex/lib 是本次产物）
    if n.startswith("META-INF/") or n == "classes.dex" or n.startswith("lib/"):
        continue
    dst.writestr(it, src.read(n)); kept += 1
dst.close()
print("  复用清单/资源条目 %d 个" % kept)
PYAXML
fi

if [ -f "$AJ" ]; then
  echo "== 3/6 javac + 自检 =="
  # R.java 只有 aapt2 link 才会生成（--java gen）。降级路径下没有它，而 apk-src/java
  # 里引用的都是 android.R.*（系统资源），所以按存在与否决定要不要带上。
  RJAVA=""
  [ -f gen/com/dsh/wb2api/R.java ] && RJAVA="gen/com/dsh/wb2api/R.java"
  javac -J-Xmx512m --release 11 -encoding UTF-8 -cp "$AJ" -d classes $RJAVA "$SRC"/java/com/dsh/wb2api/*.java
  # 看门狗退避序列自检：宿主 JVM 就能跑，不需要真机（打进 dex 就不必了，单独放 test-out）
  rm -rf test-out && mkdir -p test-out
  javac --release 11 -encoding UTF-8 -cp "$AJ:classes" -d test-out "$SRC"/test/WatchdogSelfCheck.java
  java -cp "$AJ:classes:test-out" WatchdogSelfCheck

  echo "== 4/6 dex (d8) =="
  java -cp "$R8" com.android.tools.r8.D8 --min-api 24 --lib "$AJ" --output dex \
    $(find classes -name '*.class' | tr '\n' ' ')
  find "$SRC/java" -name '*.java' -print0 | sort -z | xargs -0 cat | md5sum | cut -d' ' -f1 > "$B/.java-src-hash"
else
  echo "== 3-4/6 复用上一版 classes.dex（缺 android.jar，跳过 javac/d8/自检） =="
  # 复用前必须确认 Java 源码没改过：否则产出「新版本号 + 旧 Java 代码」，最后仍
  # exit 0 打印「完成」，装机表现就是「改了没生效」（踩过）。
  JAVA_HASH_FILE="$B/.java-src-hash"
  CUR_HASH=$(find "$SRC/java" -name '*.java' -print0 | sort -z | xargs -0 cat | md5sum | cut -d' ' -f1)
  if [ -f "$JAVA_HASH_FILE" ]; then
    OLD_HASH=$(cat "$JAVA_HASH_FILE")
    if [ "$CUR_HASH" != "$OLD_HASH" ]; then
      echo "❌ apk-src/java 在上次构建后被改过（$OLD_HASH → $CUR_HASH），但本机没有 android.jar。"
      echo "   复用旧 dex 会产出「新版本号 + 旧 Java 代码」。请先补回 $AJ 再构建。"
      exit 1
    fi
    echo "  Java 源码未变（$CUR_HASH），复用安全"
  else
    echo "⚠ 无 Java 源码指纹（首次构建），本次复用无法校验；已写入 $JAVA_HASH_FILE"
    echo "$CUR_HASH" > "$JAVA_HASH_FILE"
  fi
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
