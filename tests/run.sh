#!/data/data/com.dsharnessmobile.shell/files/usr/bin/bash
# run.sh 只跑「改动涉及的包」，全量留给发版前。
#
# 为什么：全量 11 个包 ≈ 15s（编译另算），而多数改动只碰 1-2 个包；
# 每次无脑全量是纯浪费。用法：
#   bash tests/run.sh            # 只跑 git 改动涉及的包
#   bash tests/run.sh all        # 全量（发版前跑）
#   bash tests/run.sh server pool  # 指定包
set -uo pipefail

WS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
P="$WS/workbuddy2api-panel"
PREFIX="${PREFIX:-/data/data/com.dsharnessmobile.shell/files/usr}"
export GOROOT="$PREFIX/lib/go"
export PATH="$GOROOT/bin:$PATH"
export GOFLAGS="-toolexec=/system/bin/linker64 -buildvcs=false"
# CGO 必须关：本机没有可 exec 的 aarch64-linux-android-clang（私有目录 + SELinux），
# 任何 cgo 包都会在编译期报 "fork/exec ... permission denied"。build_apk.sh 与
# rebuild.sh 早已这么设，只有本脚本漏了 —— 表现为「一半包 PASS、一半包编译失败」。
export CGO_ENABLED=0

ALL_PKGS="server upstream panel pool scheduler auth session reqlog usage redisstore credithist prompt loginsite httpauth alert media proxypool"

pick() {
  if [ "${1:-}" = "all" ]; then echo "$ALL_PKGS"; return; fi
  if [ $# -gt 0 ]; then echo "$*"; return; fi
  # 从 git 改动推包名：internal/<pkg>/... → <pkg>；cmd/... → 特殊处理
  local changed
  changed=$(cd "$WS" && git diff --name-only HEAD 2>/dev/null | sed -n 's#.*workbuddy2api-panel/internal/\([a-z]*\)/.*#\1#p' | sort -u)
  if [ -z "$changed" ]; then echo "$ALL_PKGS"; return; fi
  echo "$changed"
}

pkgs=$(pick "$@")
echo "跑: $pkgs"
fail=0
for pkg in $pkgs; do
  if ! ( cd "$P" && go test -c -o "$PREFIX/tmp/$pkg.test" "./internal/$pkg" >/dev/null 2>&1 ); then
    echo "  $pkg 编译失败"; fail=1; continue
  fi
  # -test.timeout 必给：这条路径**绕过**了 `go test`（本机只能经 linker64 跑测试二进制），
  # 而 go test 自带的默认 10 分钟超时也就一起没了 —— 一个挂住的用例会让脚本永久卡死，
  # 在 CI/发版流水线里表现为「一直不出结果」而不是「失败」。超时后 testing 会打印
  # 全部 goroutine 栈，比无限等待有用得多。
  # 判定用「输出里有一行恰好是 PASS」，不用 tail -1：包里的后台 goroutine（典型是
  # 导入后的 postImportActivate）可能在 PASS 之后补一行日志，tail -1 会把绿的判成红的
  # —— 2026-11-08 实测 panel 包就这样误报过一次。
  out=$(cd "$P/internal/$pkg" && /system/bin/linker64 "$PREFIX/tmp/$pkg.test" -test.timeout=180s 2>&1)
  if printf '%s\n' "$out" | grep -qx PASS; then
    printf "  %-11s PASS\n" "$pkg"
  else
    printf "  %-11s FAIL\n" "$pkg"
    # 先报**失败的用例名**（tail 只给最后几行日志，看不出是哪个用例红，
    # 2026-11-08 两次「panel 在 all 里红、单跑绿」都因为这条信息缺失而无法定位）。
    printf '%s\n' "$out" | grep -E "^--- FAIL|^\s+--- FAIL" | head -5 | sed 's/^/      /'
    printf '%s\n' "$out" | tail -4 | sed 's/^/      /'
    fail=1
  fi
done
# cmd/* 只在全量时跑（配置类改动才需要 cmd/server；cmd/login 有 realm/登录码用例，
# 此前一直没进过测试轮 —— 等于那批断言从来没执行过）
if [ "${1:-}" = "all" ]; then
  for c in server login; do
    if ( cd "$P" && go test -c -o "$PREFIX/tmp/cmd$c.test" "./cmd/$c" >/dev/null 2>&1 ); then
      out=$(cd "$P/cmd/$c" && /system/bin/linker64 "$PREFIX/tmp/cmd$c.test" 2>&1)
      if printf '%s\n' "$out" | grep -qx PASS; then
        printf "  %-11s PASS\n" "cmd/$c"
      else
        printf "  %-11s FAIL\n" "cmd/$c"
        printf '%s\n' "$out" | tail -4 | sed 's/^/      /'
        fail=1
      fi
    else
      printf "  %-11s %s\n" "cmd/$c" "编译失败"; fail=1
    fi
  done
fi
exit $fail
